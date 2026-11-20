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
	"mexporter/internal/session"
)

// errCaptureUnavailable marks a failure that must fall back to manual import.
// The wrapped text explains the reason to the user.
var errCaptureUnavailable = errors.New("automatic session capture is unavailable")

const (
	targetTimeout   = 20 * time.Second
	enableTimeout   = 15 * time.Second
	authWaitTimeout = 45 * time.Second
	navigateAfter   = 3 * time.Second
	pollInterval    = 250 * time.Millisecond
)

// capturedAuth collects credentials from DevTools network events.
//
// The mail API carries access_token, email and csrf_token in the query string
// of every request, so watching request URLs is enough; nothing has to be read
// out of the browser profile.
type capturedAuth struct {
	mu          sync.Mutex
	accessToken string
	email       string
	csrfToken   string
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
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.accessToken == "" {
		if v := strings.TrimSpace(q.Get("access_token")); v != "" {
			a.accessToken = v
		}
	}
	if a.email == "" {
		if v := strings.TrimSpace(q.Get("email")); v != "" {
			a.email = v
		}
	}
	if a.csrfToken == "" {
		if v := strings.TrimSpace(q.Get("csrf_token")); v != "" {
			a.csrfToken = v
		}
	}
}

func (a *capturedAuth) token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.accessToken
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

	auth := &capturedAuth{}
	conn.SetEventHandler(auth.handle)

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
	return finishCapture(conn, auth, fl.loginURL, r, out)
}

// finishCapture waits for the access token, collects cookies and validates the
// resulting session. The e-mail address is asked for only when it never showed
// up in the traffic, so the common case needs no typing at all.
func finishCapture(conn *cdp.Conn, auth *capturedAuth, loginURL string, r *bufio.Reader, out io.Writer) (session.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), authWaitTimeout)
	defer cancel()

	deadline := time.Now().Add(authWaitTimeout)
	navigateAt := time.Now().Add(navigateAfter)
	navigated := false
	for auth.token() == "" {
		if !navigated && !time.Now().Before(navigateAt) {
			navigated = true
			// Signing in does not always call the API; opening the mailbox does.
			target := inboxOf(loginURL)
			if err := conn.Call(ctx, "Page.navigate", map[string]any{"url": target}, nil); err != nil {
				logf("could not open %s to trigger the API: %v", target, err)
			}
		}
		if time.Now().After(deadline) {
			return session.Session{}, fmt.Errorf("timed out after %s waiting for the browser to authenticate", authWaitTimeout)
		}
		select {
		case <-ctx.Done():
			return session.Session{}, fmt.Errorf("timed out after %s waiting for the browser to authenticate", authWaitTimeout)
		case <-time.After(pollInterval):
		}
	}

	mailbox := auth.mailbox()
	if mailbox == "" {
		fmt.Fprint(out, "Mailbox e-mail (not seen in browser traffic): ")
		line, err := r.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return session.Session{}, fmt.Errorf("read e-mail: %w", err)
		}
		mailbox = strings.TrimSpace(line)
	}

	cookies, err := browserCookies(ctx, conn)
	if err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}
	s := session.Session{
		Email:       mailbox,
		AccessToken: auth.token(),
		CSRFToken:   auth.csrf(),
		Cookies:     cookies,
	}
	if err := session.Validate(s); err != nil {
		return session.Session{}, fmt.Errorf("%w (%v)", errCaptureUnavailable, err)
	}
	return s, nil
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

// inboxOf turns a login URL into the mailbox URL that triggers API traffic.
func inboxOf(loginURL string) string {
	u, err := url.Parse(loginURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return loginURL
	}
	u.Path = "/inbox/"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
