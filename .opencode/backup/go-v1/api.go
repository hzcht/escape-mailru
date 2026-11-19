package main

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

// flexString accepts strings and numbers in JSON.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(string(b))
	return nil
}

type Correspondent struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

type Correspondents struct {
	From []Correspondent `json:"from"`
}

func (c Correspondents) Sender() string {
	if len(c.From) == 0 {
		return ""
	}
	if c.From[0].Name != "" {
		return c.From[0].Name
	}
	return c.From[0].Email
}

type Folder struct {
	ID             flexString `json:"id"`
	Name           string     `json:"name"`
	Parent         flexString `json:"parent"`
	MessagesTotal  int        `json:"messages_total"`
	MessagesUnread int        `json:"messages_unread"`
	System         bool       `json:"system"`
	Type           string     `json:"type"`
}

type Thread struct {
	ID             string         `json:"id"`
	Length         int            `json:"length"`
	Last           string         `json:"last"`
	Expand         *string        `json:"expand"`
	Subject        string         `json:"subject"`
	Date           int64          `json:"date"`
	Folder         flexString     `json:"folder"`
	Correspondents Correspondents `json:"correspondents"`
}

type Message struct {
	ID             string         `json:"id"`
	Subject        string         `json:"subject"`
	Date           int64          `json:"date"`
	Folder         flexString     `json:"folder"`
	Correspondents Correspondents `json:"correspondents"`
}

type pinnedBox struct {
	Messages []Thread `json:"messages"`
}

type listBody struct {
	Folders      []Folder  `json:"folders"`
	Threads      []Thread  `json:"threads"`
	Pinned       pinnedBox `json:"pinned"`
	ThreadsTotal int       `json:"threads_total"`
}

type threadBody struct {
	Messages []Message `json:"messages"`
}

type apiEnvelope struct {
	Status flexString      `json:"status"`
	Body   json.RawMessage `json:"body"`
}

var (
	ErrAuth        = errors.New("authorization failed (session expired)")
	ErrRateLimited = errors.New("rate limited")
)

type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api status %d: %s", e.Status, e.Body)
}

type Client struct {
	http        *http.Client
	email       string
	accessToken string
	cookies     string
	userAgent   string
	referer     string
}

func NewClient(s Session) *Client {
	return &Client{
		http: &http.Client{
			Timeout: 120 * time.Second,
		},
		email:       s.Email,
		accessToken: s.AccessToken,
		cookies:     s.CookieHeader(),
		userAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36",
		referer:     "https://e.mail.ru/",
	}
}

func (c *Client) url(path string, params url.Values) string {
	if params == nil {
		params = url.Values{}
	}
	params.Set("email", c.email)
	params.Set("access_token", c.accessToken)
	params.Set("htmlencoded", "false")
	params.Set("_", fmt.Sprintf("%d", time.Now().UnixMilli()))
	return "https://e.mail.ru/api/v1/" + path + "?" + params.Encode()
}

func (c *Client) newRequest(ctx context.Context, rawurl string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", c.cookies)
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Referer", c.referer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	return req, nil
}

func classifyStatus(code int) error {
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

func (c *Client) getJSON(ctx context.Context, path string, params url.Values, out any) error {
	req, err := c.newRequest(ctx, c.url(path, params))
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp.StatusCode); err != nil {
		return err
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Status != "" && env.Status != "ok" && env.Status != "200" {
		return &APIError{Status: resp.StatusCode, Body: string(env.Status)}
	}
	if out == nil || len(env.Body) == 0 {
		return nil
	}
	if err := json.Unmarshal(env.Body, out); err != nil {
		return fmt.Errorf("decode body: %w", err)
	}
	return nil
}

// Folders returns the complete folder tree.
func (c *Client) Folders(ctx context.Context) ([]Folder, error) {
	params := url.Values{
		"folder":                       {"0"},
		"limit":                        {"1"},
		"filters":                      {"{}"},
		"sort":                         {`{"type":"date","order":"desc"}`},
		"last_modified":                {"1"},
		"force_custom_thread":          {"true"},
		"with_thread_representations":  {"false"},
		"supported_custom_metathreads": {`["tomyself"]`},
		"pinned_limit":                 {"50"},
		"remove_emoji_opts":            {`{"remove_from_sender_name":true,"remove_from_snippet":true,"remove_from_subject":true}`},
		"offset":                       {"0"},
	}
	var body listBody
	if err := c.getJSON(ctx, "threads/status/smart", params, &body); err != nil {
		return nil, err
	}
	return body.Folders, nil
}

// ListPage requests one page of threads for a folder.
func (c *Client) ListPage(ctx context.Context, folderID string, offset, limit int) (*listBody, error) {
	params := url.Values{
		"folder":                       {folderID},
		"limit":                        {fmt.Sprintf("%d", limit)},
		"filters":                      {"{}"},
		"sort":                         {`{"type":"date","order":"desc"}`},
		"last_modified":                {"1"},
		"force_custom_thread":          {"true"},
		"with_thread_representations":  {"false"},
		"supported_custom_metathreads": {`["tomyself"]`},
		"pinned_limit":                 {"50"},
		"remove_emoji_opts":            {`{"remove_from_sender_name":true,"remove_from_snippet":true,"remove_from_subject":true}`},
		"offset":                       {fmt.Sprintf("%d", offset)},
	}
	var body listBody
	if err := c.getJSON(ctx, "threads/status/smart", params, &body); err != nil {
		return nil, err
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
	var body threadBody
	if err := c.getJSON(ctx, "threads/thread", params, &body); err != nil {
		return nil, err
	}
	return body.Messages, nil
}

// DownloadRaw fetches the original .eml bytes for one message.
func (c *Client) DownloadRaw(ctx context.Context, messageID, folderID string) ([]byte, error) {
	params := url.Values{
		"id":        {messageID},
		"folder_id": {folderID},
	}
	req, err := c.newRequest(ctx, c.url("messages/message/download", params))
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if err := classifyStatus(resp.StatusCode); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(data) > 0 && data[0] == '{' {
		var env apiEnvelope
		if json.Unmarshal(data, &env) == nil && env.Status != "" && env.Status != "ok" && env.Status != "200" {
			return nil, &APIError{Status: resp.StatusCode, Body: string(env.Status)}
		}
	}
	return data, nil
}

// LooksLikeEml performs a cheap sanity check on downloaded bytes.
func LooksLikeEml(data []byte) bool {
	if len(data) < 100 {
		return false
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	s := string(head)
	if strings.HasPrefix(s, "Delivered-To:") || strings.HasPrefix(s, "Return-path:") || strings.HasPrefix(s, "Return-Path:") || strings.HasPrefix(s, "Received:") || strings.HasPrefix(s, "From:") {
		return true
	}
	return strings.Contains(s, "\nReceived:") || strings.Contains(s, "\nMessage-ID:") || strings.Contains(s, "\nSubject:")
}
