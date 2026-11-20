package mailapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	old := retryFirstBackoff
	retryFirstBackoff = time.Millisecond
	code := m.Run()
	retryFirstBackoff = old
	os.Exit(code)
}

func testServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(Options{
		Email:        "user@mail_ru_hosted_domain",
		AccessToken:  "token",
		CookieHeader: "s=1; t=2",
		UserAgent:    "test-agent",
		BaseURL:      srv.URL + "/api/v1",
	})
}

func TestIDUnmarshal(t *testing.T) {
	tests := []struct {
		in      string
		want    ID
		wantErr bool
	}{
		{`"42"`, "42", false},
		{`42`, "42", false},
		{`null`, "", false},
		{`""`, "", false},
		{`true`, "", true},
		{`-1`, "", true},
	}
	for _, tt := range tests {
		var v struct {
			ID ID `json:"id"`
		}
		err := json.Unmarshal([]byte(`{"id":`+tt.in+`}`), &v)
		if (err != nil) != tt.wantErr {
			t.Fatalf("%s: err=%v wantErr=%v", tt.in, err, tt.wantErr)
		}
		if !tt.wantErr && v.ID != tt.want {
			t.Fatalf("%s: id=%q want %q", tt.in, v.ID, tt.want)
		}
	}
}

func TestSender(t *testing.T) {
	if got := (Correspondents{}).Sender(); got != "" {
		t.Fatalf("empty sender = %q", got)
	}
	if got := (Correspondents{From: []Correspondent{{Name: "Name", Email: "a@b"}}}).Sender(); got != "Name" {
		t.Fatalf("name sender = %q", got)
	}
	if got := (Correspondents{From: []Correspondent{{Email: "a@b"}}}).Sender(); got != "a@b" {
		t.Fatalf("email sender = %q", got)
	}
}

func TestFoldersAndHeaders(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/threads/status/smart" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("folder") != "0" || q.Get("limit") != "1" || q.Get("access_token") != "token" || q.Get("email") != "user@mail_ru_hosted_domain" {
			t.Errorf("query = %v", q)
		}
		if q.Get("htmlencoded") != "false" || q.Get("_") == "" {
			t.Errorf("common params = %v", q)
		}
		if r.Header.Get("Cookie") != "s=1; t=2" || r.Header.Get("User-Agent") != "test-agent" {
			t.Errorf("headers = %v", r.Header)
		}
		_, _ = w.Write([]byte(`{"status":200,"body":{"folders":[{"id":0,"name":"Inbox"},{"id":"2","name":"child","parent":0}]}}`))
	})
	folders, err := client.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 2 || folders[0].ID != "0" || folders[1].Parent != "0" {
		t.Fatalf("folders = %+v", folders)
	}
}

func TestProbe(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/threads/status/smart" || r.URL.Query().Get("limit") != "1" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":200,"email":" canonical@example.test ","body":{}}`))
	})
	email, err := client.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if email != "canonical@example.test" {
		t.Fatalf("email = %q", email)
	}
}

func TestProbeRejectsExpiredSession(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":403,"email":"user@mail_ru_hosted_domain","body":"token"}`))
	})
	if _, err := client.Probe(context.Background()); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want ErrAuth", err)
	}
}

func TestFoldersErrors(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"ok","body":{}}`))
		})
		if _, err := c.Folders(context.Background()); err == nil {
			t.Fatal("expected empty error")
		}
	})
	t.Run("bad json", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{`))
		})
		if _, err := c.Folders(context.Background()); err == nil {
			t.Fatal("expected decode error")
		}
	})
	t.Run("auth envelope", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"status":"403","body":{}}`))
		})
		if _, err := c.Folders(context.Background()); !errors.Is(err, ErrAuth) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestListPageOrder(t *testing.T) {
	var gotSort string
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotSort = r.URL.Query().Get("sort")
		_, _ = w.Write([]byte(`{"status":"ok","body":{"threads":[{"id":"2:x:0","length":1,"last":"m1"}]}}`))
	})
	body, err := client.ListPage(context.Background(), "0", 50, 50, OrderAsc)
	if err != nil {
		t.Fatal(err)
	}
	if len(body.Threads) != 1 || body.Threads[0].Last != "m1" {
		t.Fatalf("body = %+v", body)
	}
	if !strings.Contains(gotSort, `"order":"asc"`) {
		t.Fatalf("sort = %s", gotSort)
	}
}

func TestThreadMessages(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/threads/thread" || r.URL.Query().Get("id") != "2:x:0" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"status":"ok","body":{"messages":[{"id":"m1"},{"id":"m2"}]}}`))
	})
	msgs, err := client.ThreadMessages(context.Background(), "2:x:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1].ID != "m2" {
		t.Fatalf("messages = %+v", msgs)
	}
}

func TestDownloadRaw(t *testing.T) {
	eml := []byte("Delivered-To: user@mail_ru_hosted_domain\r\nSubject: test\r\n\r\nbody\r\n" + strings.Repeat("x", 120))
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/messages/message/download" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("id") != "m1" || q.Get("folder_id") != "0" {
			t.Errorf("query = %v", q)
		}
		_, _ = w.Write(eml)
	})
	got, err := client.DownloadRaw(context.Background(), "m1", "0")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(eml) {
		t.Fatal("download bytes changed")
	}
}

func TestDownloadRawEnvelopeError(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":403,"body":{}}`))
	})
	if _, err := client.DownloadRaw(context.Background(), "m1", "0"); !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	tests := []struct {
		code int
		want error
	}{
		{http.StatusUnauthorized, ErrAuth},
		{http.StatusForbidden, ErrAuth},
		{http.StatusTooManyRequests, ErrRateLimited},
		{http.StatusServiceUnavailable, ErrRateLimited},
		{http.StatusInternalServerError, &APIError{Status: 500}},
	}
	for _, tt := range tests {
		client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tt.code)
		})
		_, err := client.Folders(context.Background())
		if tt.want == ErrAuth || tt.want == ErrRateLimited {
			if !errors.Is(err, tt.want) {
				t.Fatalf("%d: err = %v", tt.code, err)
			}
			continue
		}
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != tt.code {
			t.Fatalf("%d: err = %v", tt.code, err)
		}
	}
}

func TestRetryTransientHTTP(t *testing.T) {
	var attempts atomic.Int32
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","body":{"folders":[{"id":0,"name":"Inbox"}]}}`))
	})
	folders, err := client.Folders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || attempts.Load() != 3 {
		t.Fatalf("folders=%+v attempts=%d", folders, attempts.Load())
	}
}

func TestRetryRateLimitHeader(t *testing.T) {
	var attempts atomic.Int32
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","body":{"folders":[{"id":0,"name":"Inbox"}]}}`))
	})
	if _, err := client.Folders(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestRetryAfterParsing(t *testing.T) {
	seconds := &http.Response{Header: http.Header{"Retry-After": {"7"}}}
	if got := retryAfter(seconds); got != 7*time.Second {
		t.Fatalf("seconds = %s", got)
	}
	when := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	date := &http.Response{Header: http.Header{"Retry-After": {when}}}
	if got := retryAfter(date); got < 25*time.Second || got > 31*time.Second {
		t.Fatalf("date = %s", got)
	}
	empty := &http.Response{Header: http.Header{}}
	if got := retryAfter(empty); got != 0 {
		t.Fatalf("empty = %s", got)
	}
}

func TestNoRetryPermanent4xx(t *testing.T) {
	var attempts atomic.Int32
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("missing"))
	})
	_, err := client.Folders(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusNotFound || !strings.Contains(apiErr.Body, "missing") {
		t.Fatalf("err = %v", err)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d", attempts.Load())
	}
}

func TestLookalikeJSONAndCanceledContext(t *testing.T) {
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok","body":{}}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Folders(ctx); err == nil {
		t.Fatal("expected canceled error")
	}
}

func TestBuildURL(t *testing.T) {
	c := New(Options{Email: "e", AccessToken: "t", BaseURL: "https://example.test/api/"})
	raw := c.buildURL("x", url.Values{"a": {"b"}})
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/api/x" || u.Query().Get("a") != "b" || u.Query().Get("access_token") != "t" {
		t.Fatalf("url = %s", raw)
	}
}

func TestBuildURLPrefersWebToken(t *testing.T) {
	c := New(Options{Email: "e", AccessToken: "a", Token: "w", BaseURL: "https://example.test/api/"})
	u, err := url.Parse(c.buildURL("x", nil))
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("token") != "w" {
		t.Fatalf("token = %q", u.Query().Get("token"))
	}
	if u.Query().Has("access_token") {
		t.Fatalf("access_token must not be sent next to token: %s", u.RawQuery)
	}
}

func TestFetchWebToken(t *testing.T) {
	const page = `<html><head><script nonce="x" type="application/json" id="sota.config" crossorigin="anonymous">
{"userConfig":{"authCSRFToken":"csrf","api":[{"method":"/api/v1/user/short","data":{"status":"200","email":"user@example.test","body":{"token":"web-token"}}},{"method":"/api/v1/helpers","data":{"status":"200","body":[{"index":5}]}}]}}
</script></head></html>`
	client := testServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/inbox/" || r.URL.Query().Get("wv") != "1" {
			t.Errorf("request = %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		if r.Header.Get("Cookie") != "s=1; t=2" {
			t.Errorf("cookie = %q", r.Header.Get("Cookie"))
		}
		_, _ = w.Write([]byte(page))
	})
	token, email, err := client.FetchWebToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "web-token" || email != "user@example.test" {
		t.Fatalf("token = %q, email = %q", token, email)
	}
}

func TestFetchWebTokenErrors(t *testing.T) {
	t.Run("no bootstrap", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html></html>`))
		})
		if _, _, err := c.FetchWebToken(context.Background()); err == nil {
			t.Fatal("expected missing bootstrap error")
		}
	})
	t.Run("no token", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<script id="sota.config" type="application/json">{"userConfig":{"api":[{"method":"/api/v1/user/short","data":{"status":"200","body":{}}}]}}</script>`))
		})
		if _, _, err := c.FetchWebToken(context.Background()); err == nil {
			t.Fatal("expected missing token error")
		}
	})
	t.Run("signed out", func(t *testing.T) {
		c := testServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		})
		if _, _, err := c.FetchWebToken(context.Background()); !errors.Is(err, ErrAuth) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLooksLikeEml(t *testing.T) {
	tests := []struct {
		data []byte
		want bool
	}{
		{[]byte("short"), false},
		{[]byte("Delivered-To: x\r\n" + strings.Repeat("a", 200)), true},
		{[]byte("Return-path: <x>\r\n" + strings.Repeat("a", 200)), true},
		{[]byte("From: x\r\n" + strings.Repeat("a", 200)), true},
		{[]byte("junk\r\nReceived: x\r\n" + strings.Repeat("a", 200)), true},
		{[]byte(strings.Repeat("a", 300)), false},
	}
	for _, tt := range tests {
		if got := LooksLikeEml(tt.data); got != tt.want {
			t.Fatalf("LooksLikeEml(%q) = %v", tt.data[:20], got)
		}
	}
}

func TestAPIErrorString(t *testing.T) {
	err := &APIError{Status: 500, Body: "bad"}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("error = %q", err.Error())
	}
}
