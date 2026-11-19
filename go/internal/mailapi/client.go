package mailapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
)

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

// Options configures a Client. BaseURL exists for tests.
type Options struct {
	Email        string
	AccessToken  string
	CookieHeader string
	UserAgent    string
	BaseURL      string
}

// Client talks to the Mail.ru web API used by the VK WorkSpace UI.
type Client struct {
	http        *http.Client
	email       string
	accessToken string
	cookies     string
	userAgent   string
	baseURL     string
}

// New builds a client. The caller owns retries and rate limiting.
func New(o Options) *Client {
	base := strings.TrimRight(o.BaseURL, "/")
	if base == "" {
		base = strings.TrimRight(defaultBaseURL, "/")
	}
	ua := o.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}
	return &Client{
		http:        &http.Client{},
		email:       o.Email,
		accessToken: o.AccessToken,
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
	q.Set("access_token", c.accessToken)
	q.Set("htmlencoded", "false")
	q.Set("_", fmt.Sprintf("%d", time.Now().UnixMilli()))
	return c.baseURL + "/" + strings.TrimLeft(path, "/") + "?" + q.Encode()
}

// getBytes performs a bounded GET and classifies HTTP errors.
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
	if err := classifyHTTP(resp.StatusCode); err != nil {
		return nil, err
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

// classifyHTTP maps HTTP status codes to sentinel errors.
func classifyHTTP(code int) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return ErrAuth
	case code == http.StatusTooManyRequests || code == http.StatusServiceUnavailable:
		return ErrRateLimited
	case code >= 400:
		return &APIError{Status: code}
	}
	return nil
}

// classifyEnvelope maps the API-level status to sentinel errors.
func classifyEnvelope(status flexString) error {
	switch string(status) {
	case "", "ok", "200":
		return nil
	case "401", "403":
		return ErrAuth
	case "429", "503":
		return ErrRateLimited
	default:
		return &APIError{Status: http.StatusOK, Body: string(status)}
	}
}

type envelope struct {
	Status flexString      `json:"status"`
	Body   json.RawMessage `json:"body"`
}

// getJSON performs a GET and decodes the envelope and its body.
func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
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
	if out == nil || len(env.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Body, out); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
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
	raw, err := c.getBytes(ctx, "messages/message/download", params, downloadTimeout, maxMessageBytes)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", messageID, err)
	}
	if len(raw) > 0 && raw[0] == '{' {
		var env envelope
		if json.Unmarshal(raw, &env) == nil {
			if err := classifyEnvelope(env.Status); err != nil {
				return nil, fmt.Errorf("download %s: %w", messageID, err)
			}
		}
	}
	return raw, nil
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
