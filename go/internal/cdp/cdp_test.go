package cdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestAcceptKey(t *testing.T) {
	// RFC 6455 section 1.3 example.
	if got, want := acceptKey("dGhlIHNhbXBsZSBub25jZQ=="), "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="; got != want {
		t.Fatalf("acceptKey = %q, want %q", got, want)
	}
}

func TestDialWSRejectsScheme(t *testing.T) {
	if _, err := dialWS(context.Background(), "http://127.0.0.1/"); err == nil {
		t.Fatal("http scheme should be rejected")
	}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wa := &wsConn{conn: a, br: bufio.NewReader(a)}
	wb := &wsConn{conn: b, br: bufio.NewReader(b)}

	payloads := [][]byte{
		[]byte("hi"),
		make([]byte, 200),
		make([]byte, 70000),
	}
	writeErr := make(chan error, 1)
	go func() {
		for _, p := range payloads {
			if err := wa.writeFrame(0x1, p); err != nil {
				writeErr <- err
				return
			}
		}
		writeErr <- nil
	}()

	for i, want := range payloads {
		fin, opcode, got, err := wb.readFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !fin || opcode != 0x1 {
			t.Fatalf("frame %d: fin = %v, opcode = %d", i, fin, opcode)
		}
		if len(got) != len(want) {
			t.Fatalf("frame %d: len = %d, want %d", i, len(got), len(want))
		}
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
}

func TestReadLoopReassemblesFragments(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	wb := &wsConn{conn: b, br: bufio.NewReader(b)}

	msgs := make(chan []byte, 1)
	go func() {
		_ = wb.readLoop(func(p []byte) { msgs <- p })
	}()

	writeRawFrame(t, a, 0x00, 0x1, []byte("hel"))
	writeRawFrame(t, a, 0x80, 0x0, []byte("lo"))
	_ = a.Close()

	if got := <-msgs; string(got) != "hello" {
		t.Fatalf("message = %q, want %q", got, "hello")
	}
}

// writeRawFrame sends one masked frame with an explicit fin bit.
func writeRawFrame(t *testing.T, w io.Writer, fin, opcode byte, payload []byte) {
	t.Helper()
	mask := [4]byte{1, 2, 3, 4}
	head := []byte{fin | opcode, byte(0x80 | len(payload))}
	head = append(head, mask[:]...)
	for i, b := range payload {
		head = append(head, b^mask[i&3])
	}
	if _, err := w.Write(head); err != nil {
		t.Fatal(err)
	}
}

func TestConnCallRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	srv := &wsConn{conn: a, br: bufio.NewReader(a)}
	conn := &Conn{
		ws:      &wsConn{conn: b, br: bufio.NewReader(b)},
		waiters: make(map[int]chan callResult),
		done:    make(chan struct{}),
	}
	events := make(chan string, 1)
	conn.SetEventHandler(func(method string, _ json.RawMessage) { events <- method })
	go conn.loop()

	go func() {
		_, _, payload, err := srv.readFrame()
		if err != nil {
			return
		}
		var cmd message
		if err := json.Unmarshal(payload, &cmd); err != nil {
			return
		}
		reply, _ := json.Marshal(message{ID: cmd.ID, Result: json.RawMessage(`{"cookies":["a"]}`)})
		if err := srv.writeFrame(0x1, reply); err != nil {
			return
		}
		event, _ := json.Marshal(message{Method: "Network.requestWillBeSent", Params: json.RawMessage(`{}`)})
		_ = srv.writeFrame(0x1, event)
	}()

	var out struct {
		Cookies []string `json:"cookies"`
	}
	if err := conn.Call(context.Background(), "Storage.getCookies", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Cookies) != 1 || out.Cookies[0] != "a" {
		t.Fatalf("result = %+v", out)
	}
	select {
	case method := <-events:
		if method != "Network.requestWillBeSent" {
			t.Fatalf("event = %q", method)
		}
	case <-time.After(time.Second):
		t.Fatal("event handler was not called")
	}
}

func TestPickTargetPrefersLoginOrigin(t *testing.T) {
	targets := []target{
		{Type: "page", URL: "https://other.example/", WebSocketDebuggerURL: "ws://127.0.0.1:1/devtools/page/other"},
		{Type: "page", URL: "https://e.mail.ru/inbox/", WebSocketDebuggerURL: "ws://127.0.0.1:1/devtools/page/mail"},
		{Type: "background", URL: "https://e.mail.ru/", WebSocketDebuggerURL: "ws://127.0.0.1:1/devtools/page/bg"},
	}
	got, ok := pickTarget(targets, "https://e.mail.ru/")
	if !ok || got != "ws://127.0.0.1:1/devtools/page/mail" {
		t.Fatalf("pickTarget = %q, %v", got, ok)
	}
	if _, ok := pickTarget(nil, "https://e.mail.ru/"); ok {
		t.Fatal("pickTarget with no targets should fail")
	}
}

func TestSameOrigin(t *testing.T) {
	if !sameOrigin("https://e.mail.ru/inbox/", "https://e.mail.ru/") {
		t.Fatal("same host should match")
	}
	if sameOrigin("https://e.mail.ru/", "https://vk.ru/") {
		t.Fatal("different host should not match")
	}
	if sameOrigin(":::bad", "https://e.mail.ru/") {
		t.Fatal("invalid URL should not match")
	}
}

func TestResolveBrowserExplicitPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-chrome")
	if _, err := resolveBrowser(missing); err == nil {
		t.Fatal("missing browser should fail")
	}
	bin := filepath.Join(t.TempDir(), "chrome.exe")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveBrowser(bin)
	if err != nil || got != bin {
		t.Fatalf("resolveBrowser = %q, %v", got, err)
	}
}

func TestLaunchRequiresLoginURL(t *testing.T) {
	if _, err := Launch(LaunchOptions{}); err == nil {
		t.Fatal("Launch without a login URL should fail")
	}
}

func TestWaitPortReadsDevToolsActivePort(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("9222\n/devtools/browser/abc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (&Browser{profileDir: dir}).waitPort(time.Second)
	if err != nil || got != 9222 {
		t.Fatalf("waitPort = %d, %v", got, err)
	}
}

func TestWaitPortTimesOut(t *testing.T) {
	if _, err := (&Browser{profileDir: t.TempDir()}).waitPort(150 * time.Millisecond); err == nil {
		t.Fatal("waitPort without the port file should time out")
	}
}

func TestLaunchRejectsMissingBrowser(t *testing.T) {
	_, err := Launch(LaunchOptions{
		LoginURL:    "https://e.mail.ru/",
		BrowserPath: filepath.Join(t.TempDir(), "missing-chrome"),
	})
	if err == nil {
		t.Fatal("Launch with a missing browser should fail")
	}
}

func TestTargetUsesDevToolsEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/list" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `[{"type":"page","url":"https://e.mail.ru/","webSocketDebuggerUrl":"ws://127.0.0.1:9/devtools/page/1"}]`)
	}))
	defer srv.Close()

	b := &Browser{port: portOf(t, srv.URL), loginURL: "https://e.mail.ru/"}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := b.Target(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := "ws://127.0.0.1:9/devtools/page/1"; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
}

func TestPageTargetsListsOnlyPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/list" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `[
			{"type":"page","url":"https://e.mail.ru/inbox/","webSocketDebuggerUrl":"ws://127.0.0.1:9/devtools/page/1"},
			{"type":"page","url":"https://id.vk.ru/","webSocketDebuggerUrl":"ws://127.0.0.1:9/devtools/page/2"},
			{"type":"background","url":"https://e.mail.ru/","webSocketDebuggerUrl":"ws://127.0.0.1:9/devtools/page/3"}
		]`)
	}))
	defer srv.Close()

	b := &Browser{port: portOf(t, srv.URL)}
	targets, err := b.PageTargets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[1].URL != "https://id.vk.ru/" {
		t.Fatalf("targets = %+v", targets)
	}
}

func TestTargetReportsMissingTargets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	b := &Browser{port: portOf(t, srv.URL), loginURL: "https://e.mail.ru/"}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := b.Target(ctx); err == nil {
		t.Fatal("Target with no page targets should fail")
	}
}

func TestFetchTargetsRejectsBadStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fetchTargets(srv.Client(), req); err == nil {
		t.Fatal("fetchTargets on a 500 response should fail")
	}
}

func TestDialWSHandshakeError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
	}()

	_, err = dialWS(context.Background(), "ws://"+ln.Addr().String()+"/devtools/page/1")
	if err == nil {
		t.Fatal("handshake against a non-websocket server should fail")
	}
}

func TestConnCallAfterClose(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	conn := &Conn{
		ws:      &wsConn{conn: b, br: bufio.NewReader(b)},
		waiters: make(map[int]chan callResult),
		done:    make(chan struct{}),
	}
	go conn.loop()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	if err := conn.Call(context.Background(), "Network.enable", nil, nil); err == nil {
		t.Fatal("Call after Close should fail")
	}
}

func TestCloseRemovesThrowawayProfile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	b := &Browser{profileDir: profile, removeProfile: true}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile still exists: %v", err)
	}
}

func TestCloseWithoutProcessIsNoop(t *testing.T) {
	b := &Browser{profileDir: t.TempDir(), removeProfile: false}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
}

// portOf extracts the port of a httptest server URL.
func portOf(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestPort(t *testing.T) {
	if got := (&Browser{port: 9222}).Port(); got != 9222 {
		t.Fatalf("Port = %d", got)
	}
}

func TestClosedError(t *testing.T) {
	c := &Conn{}
	if got := c.closedError().Error(); got != "cdp: connection closed" {
		t.Fatalf("closedError = %q", got)
	}
	c.err = errors.New("boom")
	if got := c.closedError().Error(); got != "cdp: boom" {
		t.Fatalf("closedError = %q", got)
	}
}

func TestDialAndCallOverWebSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	wsURL := "ws://" + ln.Addr().String() + "/devtools/page/1"

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		br := bufio.NewReader(conn)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		key := req.Header.Get("Sec-WebSocket-Key")
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", acceptKey(key))
		srv := &wsConn{conn: conn, br: br}
		_, _, payload, err := srv.readFrame()
		if err != nil {
			return
		}
		var cmd message
		if err := json.Unmarshal(payload, &cmd); err != nil {
			return
		}
		reply, _ := json.Marshal(message{ID: cmd.ID, Result: json.RawMessage(`{"ok":true}`)})
		_ = srv.writeFrame(0x1, reply)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := Dial(ctx, wsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	if err := conn.Call(ctx, "Network.enable", nil, &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK {
		t.Fatalf("result = %+v", out)
	}
}

func TestLaunchFailsToStartBrowser(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "not-a-browser.txt")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Launch(LaunchOptions{
		LoginURL:       "https://e.mail.ru/",
		BrowserPath:    bin,
		StartupTimeout: 500 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Launch with a non-executable file should fail")
	}
}
