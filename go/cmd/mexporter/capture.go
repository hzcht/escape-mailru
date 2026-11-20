package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync"
	"time"

	"mexporter/internal/cdp"
	"mexporter/internal/mailapi"
	"mexporter/internal/session"
)

// errCaptureUnavailable marks a failure that must fall back to manual import.
// The wrapped text explains the reason to the user.
var errCaptureUnavailable = errors.New("automatic session capture is unavailable")

const (
	targetTimeout    = 20 * time.Second
	enableTimeout    = 15 * time.Second
	authWaitTimeout  = 120 * time.Second
	pollInterval     = 250 * time.Millisecond
	validateInterval = 5 * time.Second
	defaultAPIBase   = "https://e.mail.ru/api/v1/"
)

// capturedAuth collects credentials from DevTools network events.
//
// The mail API carries access_token, email and csrf_token in the query string
// of every request, so watching request URLs is enough; nothing has to be read
// out of the browser profile.
//
// A page also issues requests with a guest token (the "anonym." prefix) before
// the user signs in, and the signed-in mailbox uses a different token. Tokens
// are therefore collected instead of remembered first-wins, and the caller
// checks them against the API rather than trusting whichever arrived first.
type capturedAuth struct {
	mu        sync.Mutex
	base      *url.URL
	observed  []string
	onAPI     map[string]bool
	hosts     map[string]string
	email     string
	csrfToken string
}

// newCapturedAuth builds a collector that recognizes requests to baseURL.
func newCapturedAuth(baseURL string) *capturedAuth {
	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = defaultAPIBase
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		u = nil
	}
	return &capturedAuth{base: u, onAPI: map[string]bool{}, hosts: map[string]string{}}
}

func (a *capturedAuth) handle(method string, params json.RawMessage) {
	if method != "Network.requestWillBeSent" {
		return
	}
	var ev struct {
		Request struct {
			URL      string `json:"url"`
			PostData string `json:"postData"`
		} `json:"request"`
	}
	if err := json.Unmarshal(params, &ev); err != nil {
		return
	}
	a.observe(ev.Request.URL)
	a.observe(ev.Request.PostData)
}

// observe reads credentials from a URL or from an encoded request body.
func (a *capturedAuth) observe(raw string) {
	if raw == "" {
		return
	}
	q := captureParams(raw)
	if q == nil {
		return
	}
	token := strings.TrimSpace(q.Get("access_token"))
	mailbox := strings.TrimSpace(q.Get("email"))
	csrf := strings.TrimSpace(q.Get("csrf_token"))
	onAPI := a.apiRequest(raw)
	host := requestHost(raw)

	a.mu.Lock()
	added := false
	if token != "" {
		if a.onAPI == nil {
			a.onAPI = map[string]bool{}
		}
		if a.hosts == nil {
			a.hosts = map[string]string{}
		}
		if !a.seen(token) {
			a.observed = append(a.observed, token)
			a.hosts[token] = host
			added = true
		}
		if onAPI {
			a.onAPI[token] = true
		}
	}
	a.mu.Unlock()
	if added {
		logf("capture: access token candidate from %s (%d chars)", host, len(token))
	}
	a.mu.Lock()
	if a.email == "" && mailbox != "" {
		a.email = mailbox
	}
	if a.csrfToken == "" && csrf != "" {
		a.csrfToken = csrf
	}
	a.mu.Unlock()
}

// requestHost names where a candidate token was observed, for diagnostics.
func requestHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "a request body"
	}
	return u.Host + u.Path
}

// hostSummary describes the distinct sources of the collected tokens.
func (a *capturedAuth) hostSummary() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	seen := make(map[string]bool, len(a.hosts))
	hosts := make([]string, 0, len(a.hosts))
	for _, host := range a.hosts {
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return "none"
	}
	return strings.Join(hosts, ", ")
}

// seen reports whether token is already collected. Callers hold a.mu.
func (a *capturedAuth) seen(token string) bool {
	for _, existing := range a.observed {
		if existing == token {
			return true
		}
	}
	return false
}

// apiRequest reports whether raw is a call to the configured mail API.
func (a *capturedAuth) apiRequest(raw string) bool {
	if a.base == nil {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	if !strings.EqualFold(u.Host, a.base.Host) {
		return false
	}
	basePath := strings.TrimRight(a.base.Path, "/")
	if basePath == "" {
		return true
	}
	return u.Path == basePath || strings.HasPrefix(u.Path, basePath+"/")
}

// guestToken reports whether a token belongs to a signed-out visitor.
func guestToken(token string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(token)), "anonym.")
}

// tokens returns the observed tokens in the order they are worth trying: the
// newest signed-in token seen on the mail API first, then other signed-in
// tokens, then guest tokens as a last resort.
func (a *capturedAuth) tokens() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var realAPI, realOther, guestAPI, guestOther []string
	seen := make(map[string]bool, len(a.observed))
	for i := len(a.observed) - 1; i >= 0; i-- {
		token := a.observed[i]
		if seen[token] {
			continue
		}
		seen[token] = true
		switch {
		case guestToken(token) && a.onAPI[token]:
			guestAPI = append(guestAPI, token)
		case guestToken(token):
			guestOther = append(guestOther, token)
		case a.onAPI[token]:
			realAPI = append(realAPI, token)
		default:
			realOther = append(realOther, token)
		}
	}
	out := make([]string, 0, len(a.observed))
	out = append(out, realAPI...)
	out = append(out, realOther...)
	out = append(out, guestAPI...)
	out = append(out, guestOther...)
	return out
}

// token is the most promising collected token, or "" before any arrives.
func (a *capturedAuth) token() string {
	tokens := a.tokens()
	if len(tokens) == 0 {
		return ""
	}
	return tokens[0]
}

func (a *capturedAuth) mailbox() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.email
}

func (a *capturedAuth) csrf() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.csrfToken
}

// captureParams parses credentials out of a URL query or a form body.
func captureParams(raw string) url.Values {
	if u, err := url.Parse(raw); err == nil && u.Scheme != "" {
		return u.Query()
	}
	if q, err := url.ParseQuery(raw); err == nil && len(q) > 0 {
		return q
	}
	return nil
}

// captureSession opens a debuggable browser, waits for the user to sign in,
// and reads the session out of the browser's own traffic. r is shared with the
// caller so that a fallback import never loses buffered input.
//
// It returns an error wrapping errCaptureUnavailable when the session could
// not be captured; the caller then falls back to manual JSON import.
func captureSession(fl cliFlags, r *bufio.Reader, out io.Writer) (session.Session, error) {
	browser, err := cdp.Launch(cdp.LaunchOptions{LoginURL: fl.loginURL})
	if err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}
	defer browser.Close()

	tctx, tcancel := context.WithTimeout(context.Background(), targetTimeout)
	wsURL, err := browser.Target(tctx)
	tcancel()
	if err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}

	conn, err := cdp.Dial(context.Background(), wsURL)
	if err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}
	defer conn.Close()

	auth := newCapturedAuth(fl.baseURL)
	conn.SetEventHandler(auth.handle)

	// Sign-in can finish in a new tab; watch every page, not just this one.
	watch := startTargetWatcher(browser, wsURL, auth)
	defer watch.stop()

	// Network must be enabled before the user signs in, otherwise the request
	// that carries the access token has already happened.
	ectx, ecancel := context.WithTimeout(context.Background(), enableTimeout)
	err = conn.Call(ectx, "Network.enable", nil, nil)
	ecancel()
	if err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}

	fmt.Fprintln(out, "A browser window with the login page is open.")
	fmt.Fprintln(out, "Sign in to the mailbox, then return here and press Enter.")
	if _, err := r.ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		return session.Session{}, fmt.Errorf("read sign-in confirmation: %w", err)
	}
	fmt.Fprintf(out, "Waiting up to %s for the signed-in mailbox traffic...\n", authWaitTimeout)
	return finishCapture(conn, auth, fl.baseURL, r, out)
}

// finishCapture waits for a token, collects cookies and tests the collected
// tokens against the mail API, retrying while the browser finishes signing in.
// The e-mail address is asked for only when it never showed up in the traffic,
// so the common case needs no typing at all.
func finishCapture(conn *cdp.Conn, auth *capturedAuth, baseURL string, r *bufio.Reader, out io.Writer) (session.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), authWaitTimeout)
	defer cancel()

	deadline := time.Now().Add(authWaitTimeout)
	mailbox := ""
	askedMailbox := false
	lastAttempt := time.Time{}
	lastWebAttempt := time.Time{}
	lastSignature := ""
	var lastErr error

	for {
		if time.Now().After(deadline) {
			return session.Session{}, captureFailure(lastErr, auth)
		}
		// The web UI bootstrap token works with cookies alone and needs no
		// sniffing, so try it first and independently of captured tokens.
		if time.Since(lastWebAttempt) >= validateInterval {
			lastWebAttempt = time.Now()
			captured, err := verifyWebCaptured(ctx, conn, baseURL)
			if err == nil {
				return captured, nil
			}
			lastErr = err
		}
		if auth.token() != "" && !askedMailbox {
			mailbox = auth.mailbox()
			if mailbox == "" {
				fmt.Fprint(out, "Mailbox e-mail (not seen in browser traffic): ")
				line, err := r.ReadString('\n')
				if err != nil && !errors.Is(err, io.EOF) {
					return session.Session{}, fmt.Errorf("read e-mail: %w", err)
				}
				mailbox = strings.TrimSpace(line)
			}
			askedMailbox = true
		}
		// A new token is worth an immediate retry; otherwise wait before
		// repeating a check the API has already rejected.
		signature := ""
		if askedMailbox && mailbox != "" {
			signature = strings.Join(auth.tokens(), "\x00")
		}
		if signature != "" && (signature != lastSignature || time.Since(lastAttempt) >= validateInterval) {
			fresh := signature != lastSignature
			lastSignature = signature
			lastAttempt = time.Now()
			captured, err := verifyCaptured(ctx, conn, auth, mailbox, baseURL)
			if err == nil {
				return captured, nil
			}
			lastErr = err
			if fresh {
				logf("capture: token candidate rejected (%v), still waiting for the signed-in one", err)
			}
		}
		select {
		case <-ctx.Done():
			return session.Session{}, captureFailure(lastErr, auth)
		case <-time.After(pollInterval):
		}
	}
}

// captureFailure explains why the browser session could not be captured and
// keeps the error recognizable so the caller can fall back to manual import.
func captureFailure(last error, auth *capturedAuth) error {
	if last != nil {
		return fmt.Errorf("%w (no captured token was accepted by the API: %v; tokens came from %s)",
			errCaptureUnavailable, last, auth.hostSummary())
	}
	return fmt.Errorf("%w (timed out after %s waiting for the browser to authenticate; tokens came from %s)",
		errCaptureUnavailable, authWaitTimeout, auth.hostSummary())
}

// verifyWebCaptured builds a session from the browser cookies and the token
// the mail UI embeds into its inbox page. This is the normal path: the API
// authorizes cookie-based sessions with that token, not with access_token.
func verifyWebCaptured(ctx context.Context, conn *cdp.Conn, baseURL string) (session.Session, error) {
	cookies, err := browserCookies(ctx, conn)
	if err != nil {
		return session.Session{}, err
	}
	candidate := session.Session{Cookies: cookies}
	cookieHeader := candidate.CookieHeader(time.Now())
	ui := mailapi.New(mailapi.Options{CookieHeader: cookieHeader, BaseURL: baseURL})
	token, email, err := ui.FetchWebToken(ctx)
	if err != nil {
		return session.Session{}, err
	}
	candidate.Email = email
	candidate.Token = token
	if err := session.Validate(candidate); err != nil {
		return session.Session{}, err
	}
	verify := mailapi.New(mailapi.Options{
		Email:        candidate.Email,
		Token:        candidate.Token,
		CookieHeader: candidate.CookieHeader(time.Now()),
		BaseURL:      baseURL,
	})
	resolved, err := verify.Probe(ctx)
	if err != nil {
		return session.Session{}, err
	}
	if resolved != "" {
		candidate.Email = resolved
	}
	return candidate, nil
}

// verifyCaptured tests every collected token with the browser's current
// cookies. The API's answer is authoritative: a session is returned only when
// it can actually list folders, and its canonical address is used when the API
// reports one.
func verifyCaptured(ctx context.Context, conn *cdp.Conn, auth *capturedAuth, mailbox, baseURL string) (session.Session, error) {
	cookies, err := browserCookies(ctx, conn)
	if err != nil {
		return session.Session{}, err
	}
	var lastErr error
	for _, token := range auth.tokens() {
		candidate := session.Session{
			Email:       mailbox,
			AccessToken: token,
			CSRFToken:   auth.csrf(),
			Cookies:     cookies,
		}
		if err := session.Validate(candidate); err != nil {
			lastErr = err
			continue
		}
		cli := mailapi.New(mailapi.Options{
			Email:        mailbox,
			AccessToken:  token,
			CookieHeader: candidate.CookieHeader(time.Now()),
			BaseURL:      baseURL,
		})
		resolved, err := cli.Probe(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		if resolved != "" {
			candidate.Email = resolved
		}
		return candidate, nil
	}
	if lastErr == nil {
		lastErr = errors.New("the browser reported no usable access token")
	}
	return session.Session{}, lastErr
}

// targetWatcher attaches to every page target the browser opens, because a
// sign-in can finish in a new tab while the tab that was launched stays signed
// out. Without this the only tokens seen belong to the guest page.
type targetWatcher struct {
	browser *cdp.Browser
	auth    *capturedAuth
	stopCh  chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	seen    map[string]bool
	conns   []*cdp.Conn
	closed  bool
}

func startTargetWatcher(browser *cdp.Browser, primaryWS string, auth *capturedAuth) *targetWatcher {
	w := &targetWatcher{
		browser: browser,
		auth:    auth,
		stopCh:  make(chan struct{}),
		done:    make(chan struct{}),
		seen:    map[string]bool{primaryWS: true},
	}
	go w.loop()
	return w
}

func (w *targetWatcher) loop() {
	defer close(w.done)
	ticker := time.NewTicker(750 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-w.stopCh:
			return
		case <-ticker.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		targets, err := w.browser.PageTargets(ctx)
		cancel()
		if err != nil {
			continue
		}
		for _, target := range targets {
			w.attach(target)
		}
	}
}

func (w *targetWatcher) attach(target cdp.PageTarget) {
	w.mu.Lock()
	if w.closed || w.seen[target.WebSocketDebuggerURL] {
		w.mu.Unlock()
		return
	}
	w.seen[target.WebSocketDebuggerURL] = true
	w.mu.Unlock()

	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := cdp.Dial(dctx, target.WebSocketDebuggerURL)
	dcancel()
	if err != nil {
		return
	}
	conn.SetEventHandler(w.auth.handle)
	ectx, ecancel := context.WithTimeout(context.Background(), enableTimeout)
	err = conn.Call(ectx, "Network.enable", nil, nil)
	ecancel()
	if err != nil {
		_ = conn.Close()
		return
	}
	logf("capture: watching another browser tab")
	w.mu.Lock()
	w.conns = append(w.conns, conn)
	w.mu.Unlock()
}

func (w *targetWatcher) stop() {
	close(w.stopCh)
	select {
	case <-w.done:
	case <-time.After(3 * time.Second):
	}
	w.mu.Lock()
	w.closed = true
	conns := w.conns
	w.conns = nil
	w.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

// devtoolsCookie mirrors a Chrome cookie as returned by DevTools.
type devtoolsCookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

// browserCookies reads every cookie from the browser and keeps the ones the
// API accepts. Both storage methods return the same shape, so the older one is
// used as a fallback for builds that dropped it.
func browserCookies(ctx context.Context, conn *cdp.Conn) ([]session.Cookie, error) {
	var res struct {
		Cookies []devtoolsCookie `json:"cookies"`
	}
	err := conn.Call(ctx, "Network.getAllCookies", nil, &res)
	if err != nil {
		res.Cookies = nil
		if err2 := conn.Call(ctx, "Storage.getCookies", nil, &res); err2 != nil {
			return nil, fmt.Errorf("read browser cookies: %w", err)
		}
	}
	out := acceptCookies(res.Cookies)
	if len(out) == 0 {
		return nil, errors.New("the browser reported no mail.ru or VK cookies; sign in first")
	}
	return out, nil
}

// acceptCookies keeps the cookies the mail API accepts and drops the rest.
func acceptCookies(in []devtoolsCookie) []session.Cookie {
	out := make([]session.Cookie, 0, len(in))
	for _, c := range in {
		if c.Name == "" || !session.AcceptsDomain(c.Domain) {
			continue
		}
		out = append(out, session.Cookie{
			Name:    c.Name,
			Value:   c.Value,
			Domain:  c.Domain,
			Path:    c.Path,
			Expires: c.Expires,
		})
	}
	return out
}
