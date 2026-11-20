package mailapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL   = "https://e.mail.ru/api/v1/"
	defaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"
	defaultReferer   = "https://e.mail.ru/"
	listTimeout      = 90 * time.Second
	threadTimeout    = 90 * time.Second
	downloadTimeout  = 10 * time.Minute
	maxJSONBytes     = 32 << 20
	maxMessageBytes  = 128 << 20
	maxUIBytes       = 8 << 20
	maxRetries       = 3
	retryFactor      = 3
	errorBodyBytes   = 4 << 10
)

var retryFirstBackoff = time.Second

var (
	// ErrAuth means the session expired or was rejected.
	ErrAuth = errors.New("authorization failed (session expired)")
	// ErrRateLimited means the server asked the client to slow down.
	ErrRateLimited = errors.New("rate limited")
)

// APIError is a non-authorization error returned by the mail API.
type APIError struct {
	Status int
	Body   string
}

// Error implements error.
func (e *APIError) Error() string {
	return fmt.Sprintf("api status %d: %s", e.Status, e.Body)
}

// RateLimitError carries the server's Retry-After hint while still matching
// ErrRateLimited through errors.Is.
type RateLimitError struct {
	RetryAfter time.Duration
}

// Error implements error.
func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("rate limited (retry after %s)", e.RetryAfter)
	}
	return "rate limited"
}

// Is lets callers keep using errors.Is(err, ErrRateLimited).
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Options configures a Client. BaseURL exists for tests.
type Options struct {
	Email        string
	AccessToken  string
	Token        string
	CookieHeader string
	UserAgent    string
	BaseURL      string
}

// Client talks to the Mail.ru web API used by the VK WorkSpace UI.
type Client struct {
	http        *http.Client
	email       string
	accessToken string
	token       string
	cookies     string
	userAgent   string
	baseURL     string
}

// New builds a client. The client retries transient transport failures and
// rate limits internally; callers only handle the final error.
func New(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = strings.TrimRight(defaultBaseURL, "/")
	}
	ua := o.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 100
	transport.MaxIdleConnsPerHost = 16
	transport.IdleConnTimeout = 90 * time.Second
	return &Client{
		http:        &http.Client{Transport: transport},
		email:       o.Email,
		accessToken: o.AccessToken,
		token:       o.Token,
		cookies:     o.CookieHeader,
		userAgent:   ua,
		baseURL:     base,
	}
}

// buildURL appends credentials and cache busting to an API path.
func (c *Client) buildURL(path string, params url.Values) string {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	q.Set("email", c.email)
	if c.token != "" {
		q.Set("token", c.token)
	} else {
		q.Set("access_token", c.accessToken)
	}
	q.Set("htmlencoded", "false")
	q.Set("_", fmt.Sprintf("%d", time.Now().UnixMilli()))
	return c.baseURL + "/" + strings.TrimLeft(path, "/") + "?" + q.Encode()
}

// getBytes performs one bounded GET and classifies HTTP errors.
func (c *Client) getBytes(ctx context.Context, path string, params url.Values, timeout time.Duration, limit int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.buildURL(path, params), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Cookie", c.cookies)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", defaultReferer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http get %s: %w", path, err)
	}
	// NOERR: the body is read below; a close error cannot change the result.
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyBytes))
		return nil, classifyHTTP(resp.StatusCode, string(body), retryAfter(resp))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("%s: response exceeds %d bytes", path, limit)
	}
	return raw, nil
}

// retryAfter parses the Retry-After header in seconds or HTTP-date form.
func retryAfter(resp *http.Response) time.Duration {
	value := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		if seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if when, err := http.ParseTime(value); err == nil {
		if d := time.Until(when); d > 0 {
			return d
		}
	}
	return 0
}

// classifyHTTP maps HTTP status codes to sentinel errors.
func classifyHTTP(code int, body string, wait time.Duration) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrAuth
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		return &RateLimitError{RetryAfter: wait}
	case code >= 400:
		return &APIError{Status: code, Body: strings.TrimSpace(body)}
	}
	return nil
}

// classifyEnvelope maps the API-level status to sentinel errors.
func classifyEnvelope(status flexString) error {
	s := strings.TrimSpace(string(status))
	switch s {
	case "", "ok", "200":
		return nil
	case "401", "403":
		return ErrAuth
	case "429", "503":
		return &RateLimitError{}
	default:
		code, err := strconv.Atoi(s)
		if err != nil || code < 400 {
			code = http.StatusBadRequest
		}
		return &APIError{Status: code, Body: s}
	}
}

type envelope struct {
	Status flexString      `json:"status"`
	Email  string          `json:"email"`
	Body   json.RawMessage `json:"body"`
}

// getJSON performs a GET and decodes the envelope and its body. Transient
// transport and API errors are retried by the client.
func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	var body json.RawMessage
	err := retry(ctx, func() error {
		raw, err := c.getBytes(ctx, path, params, listTimeout, maxJSONBytes)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("decode envelope: %w", err)
		}
		if err := classifyEnvelope(env.Status); err != nil {
			return err
		}
		body = env.Body
		return nil
	})
	if err != nil {
		return err
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// retry runs fn with bounded exponential backoff. Authentication errors,
// permanent 4xx responses and caller cancellation are not retried, and the
// final failure is not followed by an unnecessary sleep.
func retry(ctx context.Context, fn func() error) error {
	backoff := retryFirstBackoff
	var last error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if terminalError(err) {
			return err
		}
		if attempt == maxRetries {
			break
		}
		pause := backoff
		var limited *RateLimitError
		if errors.As(err, &limited) && limited.RetryAfter > 0 {
			pause = limited.RetryAfter
		}
		if err := sleepCtx(ctx, pause); err != nil {
			return err
		}
		backoff *= retryFactor
	}
	return last
}

func terminalError(err error) bool {
	if errors.Is(err, ErrAuth) || errors.Is(err, context.Canceled) {
		return true
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Status >= 400 && apiErr.Status < 500 && apiErr.Status != http.StatusRequestTimeout && apiErr.Status != http.StatusTooManyRequests
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// listParams builds the shared threads/status/smart query.
func listParams(folderID string, offset, limit int, order Order) url.Values {
	if order != OrderAsc {
		order = OrderDesc
	}
	return url.Values{
		"folder":                       {folderID},
		"limit":                        {fmt.Sprintf("%d", limit)},
		"offset":                       {fmt.Sprintf("%d", offset)},
		"filters":                      {"{}"},
		"sort":                         {fmt.Sprintf(`{"type":"date","order":%q}`, order)},
		"last_modified":                {"1"},
		"force_custom_thread":          {"true"},
		"with_thread_representations":  {"false"},
		"supported_custom_metathreads": {`["tomyself"]`},
		"pinned_limit":                 {"50"},
		"remove_emoji_opts":            {`{"remove_from_sender_name":true,"remove_from_snippet":true,"remove_from_subject":true}`},
	}
}

// webBootstrap is the part of the mail UI bootstrap that this client reads.
type webBootstrap struct {
	UserConfig struct {
		CSRFToken string `json:"authCSRFToken"`
		API       []struct {
			Method string `json:"method"`
			Data   struct {
				Status flexString      `json:"status"`
				Email  string          `json:"email"`
				Body   json.RawMessage `json:"body"`
			} `json:"data"`
		} `json:"api"`
	} `json:"userConfig"`
}

var webBootstrapScript = regexp.MustCompile(`(?s)<script[^>]*id=["']sota\.config["'][^>]*>(.*?)</script>`)

// FetchWebToken reads the web-session token from the mailbox web UI bootstrap.
//
// The current mail UI authorizes API calls with a "token" query parameter that
// the server embeds into the inbox page as a prefetched /api/v1/user/short
// response. Fetching that page needs only the session cookies, so a session
// whose token expired can be refreshed without signing in again.
func (c *Client) FetchWebToken(ctx context.Context) (token, email string, err error) {
	ui, err := c.webUIURL()
	if err != nil {
		return "", "", err
	}
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ui, nil)
	if err != nil {
		return "", "", fmt.Errorf("build web UI request: %w", err)
	}
	req.Header.Set("Cookie", c.cookies)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", defaultReferer)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("open web UI: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyBytes))
		return "", "", classifyHTTP(resp.StatusCode, string(body), retryAfter(resp))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUIBytes+1))
	if err != nil {
		return "", "", fmt.Errorf("read web UI: %w", err)
	}
	if len(raw) > maxUIBytes {
		return "", "", fmt.Errorf("web UI response exceeds %d bytes", maxUIBytes)
	}
	return parseWebBootstrap(raw)
}

// webUIURL turns the API base into the inbox URL that bootstraps the web app.
func (c *Client) webUIURL() (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("web UI url: invalid base %q", c.baseURL)
	}
	u.Path = "/inbox/"
	u.RawQuery = "wv=1"
	u.Fragment = ""
	return u.String(), nil
}

// parseWebBootstrap extracts the user/short token and address from the page.
func parseWebBootstrap(html []byte) (token, email string, err error) {
	m := webBootstrapScript.FindSubmatch(html)
	if len(m) < 2 {
		return "", "", errors.New("web UI bootstrap not found")
	}
	var cfg webBootstrap
	if err := json.Unmarshal(m[1], &cfg); err != nil {
		return "", "", fmt.Errorf("decode web UI bootstrap: %w", err)
	}
	for _, entry := range cfg.UserConfig.API {
		if entry.Method != "/api/v1/user/short" {
			continue
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(entry.Data.Body, &body); err != nil {
			return "", "", fmt.Errorf("decode user/short body: %w", err)
		}
		token = strings.TrimSpace(body.Token)
		email = strings.TrimSpace(entry.Data.Email)
		if token == "" {
			return "", "", errors.New("web UI bootstrap has no token")
		}
		return token, email, nil
	}
	return "", "", errors.New("web UI bootstrap has no user/short response")
}

// Probe checks the credentials with the cheapest list call and returns the
// canonical mailbox address the API resolved them to. The address is empty
// when the response does not report one.
func (c *Client) Probe(ctx context.Context) (string, error) {
	var email string
	err := retry(ctx, func() error {
		raw, err := c.getBytes(ctx, "threads/status/smart", listParams("0", 0, 1, OrderDesc), listTimeout, maxJSONBytes)
		if err != nil {
			return err
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			return fmt.Errorf("decode envelope: %w", err)
		}
		if err := classifyEnvelope(env.Status); err != nil {
			return err
		}
		email = strings.TrimSpace(env.Email)
		return nil
	})
	if err != nil {
		return "", err
	}
	return email, nil
}

// Folders returns the complete folder tree.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	var body ListBody
	if err := c.getJSON(ctx, "threads/status/smart", listParams("0", 0, 1, OrderDesc), &body); err != nil {
		return nil, fmt.Errorf("list folders: %w", err)
	}
	if len(body.Folders) == 0 {
		return nil, fmt.Errorf("list folders: empty response")
	}
	return body.Folders, nil
}

// ListPage returns one page of threads for a folder.
//
// REQ-EXPORT-001: OrderAsc is used for exports so new mail does not shift offsets.
func (c *Client) ListPage(ctx context.Context, folderID string, offset, limit int, order Order) (*ListBody, error) {
	var body ListBody
	params := listParams(folderID, offset, limit, order)
	if err := c.getJSON(ctx, "threads/status/smart", params, &body); err != nil {
		return nil, fmt.Errorf("list folder %s offset %d: %w", folderID, offset, err)
	}
	return &body, nil
}

// ThreadMessages expands a multi-message thread.
func (c *Client) ThreadMessages(ctx context.Context, threadID string) ([]Message, error) {
	params := url.Values{
		"id":                  {threadID},
		"force_custom_thread": {"true"},
		"use_color_scheme":    {"1"},
	}
	var body struct {
		Messages []Message `json:"messages"`
	}
	ctx, cancel := context.WithTimeout(ctx, threadTimeout)
	defer cancel()
	if err := c.getJSON(ctx, "threads/thread", params, &body); err != nil {
		return nil, fmt.Errorf("expand thread %s: %w", threadID, err)
	}
	return body.Messages, nil
}

// DownloadRaw fetches the original .eml bytes of one message.
//
// REQ-EXPORT-002: the response is returned byte-for-byte; no transcoding.
func (c *Client) DownloadRaw(ctx context.Context, messageID, folderID string) ([]byte, error) {
	params := url.Values{
		"id":        {messageID},
		"folder_id": {folderID},
	}
	var data []byte
	err := retry(ctx, func() error {
		raw, err := c.getBytes(ctx, "messages/message/download", params, downloadTimeout, maxMessageBytes)
		if err != nil {
			return err
		}
		if len(raw) > 0 && raw[0] == '{' {
			var env envelope
			if json.Unmarshal(raw, &env) == nil {
				if err := classifyEnvelope(env.Status); err != nil {
					return err
				}
			}
		}
		data = raw
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", messageID, err)
	}
	return data, nil
}

// LooksLikeEml performs a cheap header sanity check on downloaded bytes.
func LooksLikeEml(data []byte) bool {
	if len(data) < 100 {
		return false
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	s := string(head)
	for _, prefix := range []string{"Delivered-To:", "Return-path:", "Return-Path:", "Received:", "From:"} {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return strings.Contains(s, "\nReceived:") || strings.Contains(s, "\nMessage-ID:") || strings.Contains(s, "\nSubject:")
}
