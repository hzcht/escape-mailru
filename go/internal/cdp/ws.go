package cdp

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// wsGUID is the magic value from RFC 6455 section 1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// maxFrameSize keeps a runaway or hostile frame from exhausting memory. DevTools
// messages are small; 32 MiB leaves plenty of headroom.
const maxFrameSize = 32 << 20

// wsConn is a minimal RFC 6455 client. The Go standard library ships no
// WebSocket implementation and this project stays dependency free, so the
// subset DevTools needs is implemented here: text frames, fragmentation,
// ping/pong and close.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	writeMu  sync.Mutex
	closeMu  sync.Mutex
	closeErr error
	closed   bool
}

// dialWS opens a WebSocket connection to target (ws:// or wss://).
func dialWS(ctx context.Context, target string) (*wsConn, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("cdp: parse websocket url: %w", err)
	}
	switch u.Scheme {
	case "ws", "wss":
	default:
		return nil, fmt.Errorf("cdp: unsupported websocket scheme %q", u.Scheme)
	}
	host := u.Host
	if u.Port() == "" {
		if u.Scheme == "wss" {
			host = net.JoinHostPort(u.Hostname(), "443")
		} else {
			host = net.JoinHostPort(u.Hostname(), "80")
		}
	}
	var raw net.Conn
	d := net.Dialer{Timeout: 10 * time.Second}
	if u.Scheme == "wss" {
		raw, err = tls.DialWithDialer(&d, "tcp", host, &tls.Config{ServerName: u.Hostname()})
	} else {
		raw, err = d.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("cdp: connect websocket: %w", err)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = raw.SetDeadline(deadline)
	}
	conn := &wsConn{conn: raw, br: bufio.NewReader(raw)}
	if err := conn.handshake(u); err != nil {
		raw.Close()
		return nil, err
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

// handshake performs the HTTP Upgrade exchange and verifies the accept key.
func (w *wsConn) handshake(u *url.URL) error {
	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		return fmt.Errorf("cdp: websocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", path, u.Host, key)
	if _, err := io.WriteString(w.conn, req); err != nil {
		return fmt.Errorf("cdp: websocket handshake write: %w", err)
	}
	resp, err := http.ReadResponse(w.br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fmt.Errorf("cdp: websocket handshake read: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		return fmt.Errorf("cdp: websocket handshake: %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptKey(key); got != want {
		return fmt.Errorf("cdp: websocket handshake: bad accept key %q", got)
	}
	return nil
}

// acceptKey computes Sec-WebSocket-Accept per RFC 6455 section 4.2.2.
func acceptKey(key string) string {
	sum := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// writeFrame sends one masked frame. Clients must mask; servers must not.
func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	w.closeMu.Lock()
	if w.closed {
		w.closeMu.Unlock()
		return io.ErrClosedPipe
	}
	w.closeMu.Unlock()

	l := len(payload)
	head := make([]byte, 0, 14)
	head = append(head, 0x80|opcode)
	switch {
	case l <= 125:
		head = append(head, byte(0x80|l))
	case l <= 65535:
		head = append(head, 0x80|126, byte(l>>8), byte(l))
	default:
		head = append(head, 0x80|127)
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(l))
		head = append(head, ext[:]...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("cdp: mask key: %w", err)
	}
	head = append(head, mask[:]...)
	if l > 0 {
		masked := make([]byte, l)
		for i, b := range payload {
			masked[i] = b ^ mask[i&3]
		}
		head = append(head, masked...)
	}
	_, err := w.conn.Write(head)
	return err
}

// readFrame decodes one frame and unmasks its payload.
func (w *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	opcode = h[0] & 0x0f
	masked := h[1]&0x80 != 0
	l := uint64(h[1] & 0x7f)
	switch l {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(w.br, ext[:]); err != nil {
			return
		}
		l = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(w.br, ext[:]); err != nil {
			return
		}
		l = binary.BigEndian.Uint64(ext[:])
	}
	if l > maxFrameSize {
		return false, 0, nil, fmt.Errorf("cdp: frame too large: %d bytes", l)
	}
	var maskKey [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, maskKey[:]); err != nil {
			return
		}
	}
	payload = make([]byte, l)
	if l > 0 {
		if _, err = io.ReadFull(w.br, payload); err != nil {
			return
		}
		if masked {
			for i := range payload {
				payload[i] ^= maskKey[i&3]
			}
		}
	}
	return
}

// readLoop decodes frames until the connection breaks and passes each complete
// message to handle. It replies to pings and answers close frames.
func (w *wsConn) readLoop(handle func([]byte)) error {
	var frag []byte
	for {
		fin, opcode, payload, err := w.readFrame()
		if err != nil {
			return err
		}
		switch opcode {
		case 0x0: // continuation
			frag = append(frag, payload...)
			if fin {
				if handle != nil {
					handle(frag)
				}
				frag = nil
			}
		case 0x1, 0x2: // text, binary
			if !fin {
				frag = append(frag[:0], payload...)
				continue
			}
			if handle != nil {
				handle(payload)
			}
		case 0x8: // close
			_ = w.writeFrame(0x8, nil)
			return io.EOF
		case 0x9: // ping
			_ = w.writeFrame(0xA, payload)
		case 0xA: // pong
		default:
			return fmt.Errorf("cdp: unexpected websocket opcode %d", opcode)
		}
	}
}

// close shuts the underlying connection exactly once.
func (w *wsConn) close() error {
	w.closeMu.Lock()
	defer w.closeMu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	w.closeErr = w.conn.Close()
	return w.closeErr
}
