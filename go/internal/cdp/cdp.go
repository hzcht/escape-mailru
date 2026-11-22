// Package cdp drives a local Chrome or Edge instance over the Chrome DevTools
// Protocol.
//
// It exists for exactly one job: guided session setup. The browser is started
// with a throwaway profile, the user signs in by hand, and the app then reads
// the access token and cookies out of the browser's own network traffic so the
// user never has to assemble a JSON document. Nothing here talks to the mail
// API, and nothing here outlives the setup step.
package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// Conn is one DevTools protocol connection to a page target.
type Conn struct {
	ws *wsConn

	mu      sync.Mutex
	nextID  int
	waiters map[int]chan callResult
	handler func(method string, params json.RawMessage)
	err     error
	done    chan struct{}
}

type callResult struct {
	result json.RawMessage
	err    error
}

// message mirrors the DevTools wire format for both replies and events.
type message struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *callError      `json:"error,omitempty"`
}

type callError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Dial connects to a DevTools WebSocket endpoint and starts reading replies.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &Conn{
		ws:      ws,
		waiters: make(map[int]chan callResult),
		done:    make(chan struct{}),
	}
	// Lifecycle: the read loop ends when the websocket closes.
	go c.loop()
	return c, nil
}

// SetEventHandler registers the callback for unsolicited events. It must be
// called before issuing commands that produce events; the handler runs on the
// read loop and must not block.
func (c *Conn) SetEventHandler(h func(method string, params json.RawMessage)) {
	c.mu.Lock()
	c.handler = h
	c.mu.Unlock()
}

// Call sends one command and decodes its result into out when out is not nil.
// A nil params is sent as an empty object, which DevTools accepts everywhere a
// params block is optional.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	select {
	case <-c.done:
		return c.closedError()
	default:
	}

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan callResult, 1)
	c.waiters[id] = ch
	c.mu.Unlock()

	body := map[string]any{"id": id, "method": method, "params": params}
	if params == nil {
		body["params"] = struct{}{}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		c.dropWaiter(id)
		return fmt.Errorf("cdp: encode %s: %w", method, err)
	}
	if err := c.ws.writeFrameContext(ctx, 0x1, raw); err != nil {
		c.dropWaiter(id)
		return fmt.Errorf("cdp: send %s: %w", method, err)
	}

	select {
	case res := <-ch:
		if res.err != nil {
			return res.err
		}
		if out != nil && len(res.result) > 0 {
			if err := json.Unmarshal(res.result, out); err != nil {
				return fmt.Errorf("cdp: decode %s result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.dropWaiter(id)
		return ctx.Err()
	case <-c.done:
		return c.closedError()
	}
}

// loop reads messages until the connection dies and unblocks every waiter.
func (c *Conn) loop() {
	err := c.ws.readLoop(c.dispatch)
	if err != nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)) {
		err = errors.New("connection closed")
	}
	c.fail(err)
}

func (c *Conn) dispatch(raw []byte) {
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return // Not a protocol frame: nothing sensible to do with it.
	}
	if m.ID != 0 {
		c.mu.Lock()
		ch := c.waiters[m.ID]
		delete(c.waiters, m.ID)
		c.mu.Unlock()
		if ch == nil {
			return
		}
		if m.Error != nil {
			ch <- callResult{err: fmt.Errorf("cdp: %s (%d)", m.Error.Message, m.Error.Code)}
		} else {
			ch <- callResult{result: m.Result}
		}
		return
	}
	if m.Method == "" {
		return
	}
	c.mu.Lock()
	h := c.handler
	c.mu.Unlock()
	if h != nil {
		h(m.Method, m.Params)
	}
}

func (c *Conn) dropWaiter(id int) {
	c.mu.Lock()
	delete(c.waiters, id)
	c.mu.Unlock()
}

func (c *Conn) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		if err == nil {
			err = errors.New("connection closed")
		}
		c.err = err
	}
	for id, ch := range c.waiters {
		ch <- callResult{err: c.err}
		delete(c.waiters, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("cdp: %w", c.err)
	}
	return errors.New("cdp: connection closed")
}

// Close tears the connection down. The read loop then unblocks any in-flight
// Call with an error rather than hanging.
func (c *Conn) Close() error {
	return c.ws.close()
}
