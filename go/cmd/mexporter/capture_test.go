package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCaptureParams(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "query",
			raw:  "https://e.mail.ru/api/v1/?access_token=t&email=u@mail.ru",
			want: map[string]string{"access_token": "t", "email": "u@mail.ru"},
		},
		{
			name: "form",
			raw:  "access_token=t&csrf_token=c",
			want: map[string]string{"access_token": "t", "csrf_token": "c"},
		},
		{name: "empty", raw: "", want: nil},
		{name: "no params", raw: "https://e.mail.ru/", want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := captureParams(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("params = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got.Get(k) != v {
					t.Fatalf("params[%q] = %q, want %q", k, got.Get(k), v)
				}
			}
		})
	}
}

func TestCapturedAuthFromNetworkEvent(t *testing.T) {
	const event = `{"method":"Network.requestWillBeSent","params":{"request":{"url":"https://e.mail.ru/api/v1/?access_token=tok&email=u@mail.ru","postData":"csrf_token=csr"}}}`
	var m struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal([]byte(event), &m); err != nil {
		t.Fatal(err)
	}
	a := &capturedAuth{}
	a.handle(m.Method, m.Params)
	if a.token() != "tok" || a.mailbox() != "u@mail.ru" || a.csrf() != "csr" {
		t.Fatalf("auth = %q, %q, %q", a.token(), a.mailbox(), a.csrf())
	}
}

func TestCapturedAuthIgnoresOtherMethods(t *testing.T) {
	a := &capturedAuth{}
	a.handle("Network.responseReceived", json.RawMessage(`{"request":{"url":"https://e.mail.ru/?access_token=tok"}}`))
	if a.token() != "" {
		t.Fatalf("token = %q, want empty", a.token())
	}
}

func TestCapturedAuthPrefersSignedInAPIToken(t *testing.T) {
	a := newCapturedAuth("https://e.mail.ru/api/v1/")
	a.observe("https://e.mail.ru/api/v1/threads/status/smart?access_token=anonym.guest&email=u@mail.ru")
	a.observe("https://e.mail.ru/api/v1/threads/status/smart?access_token=real")
	a.observe("https://id.example.test/?access_token=other")
	tokens := a.tokens()
	want := []string{"real", "other", "anonym.guest"}
	if len(tokens) != len(want) {
		t.Fatalf("tokens = %v, want %v", tokens, want)
	}
	for i := range want {
		if tokens[i] != want[i] {
			t.Fatalf("tokens = %v, want %v", tokens, want)
		}
	}
}

func TestCapturedAuthPrefersNewestToken(t *testing.T) {
	a := newCapturedAuth("https://e.mail.ru/api/v1/")
	a.observe("https://e.mail.ru/api/v1/threads/status/smart?access_token=old")
	a.observe("https://e.mail.ru/api/v1/threads/status/smart?access_token=new")
	if got := a.token(); got != "new" {
		t.Fatalf("token = %q, want newest", got)
	}
}

func TestCapturedAuthKeepsGuestTokenAsLastResort(t *testing.T) {
	a := newCapturedAuth("https://e.mail.ru/api/v1/")
	a.observe("https://e.mail.ru/api/v1/threads/status/smart?access_token=anonym.guest")
	if got := a.token(); got != "anonym.guest" {
		t.Fatalf("token = %q, want the guest token", got)
	}
}

func TestCapturedAuthDedupesTokens(t *testing.T) {
	a := newCapturedAuth("https://e.mail.ru/api/v1/")
	a.observe("https://e.mail.ru/api/v1/x?access_token=same")
	a.observe("https://e.mail.ru/api/v1/x?access_token=same")
	if got := len(a.tokens()); got != 1 {
		t.Fatalf("token count = %d, want 1", got)
	}
}

func TestRequestHostDropsQuery(t *testing.T) {
	got := requestHost("https://e.mail.ru/api/v1/threads/status/smart?access_token=secret")
	if got != "e.mail.ru/api/v1/threads/status/smart" {
		t.Fatalf("requestHost = %q", got)
	}
	if strings.Contains(got, "secret") {
		t.Fatal("requestHost leaked the query")
	}
	if got := requestHost("access_token=secret"); got != "a request body" {
		t.Fatalf("requestHost body = %q", got)
	}
}

func TestAcceptCookiesFiltersDomains(t *testing.T) {
	got := acceptCookies([]devtoolsCookie{
		{Name: "s", Value: "1", Domain: ".mail.ru", Path: "/"},
		{Name: "", Value: "x", Domain: ".mail.ru"},
		{Name: "tracker", Value: "1", Domain: ".example.com"},
	})
	if len(got) != 1 || got[0].Name != "s" {
		t.Fatalf("cookies = %+v", got)
	}
	if len(acceptCookies(nil)) != 0 {
		t.Fatal("nil input should give no cookies")
	}
}

func TestCaptureSessionRequiresLoginURL(t *testing.T) {
	_, err := captureSession(cliFlags{}, bufio.NewReader(strings.NewReader("")), io.Discard)
	if !errors.Is(err, errCaptureUnavailable) {
		t.Fatalf("err = %v, want errCaptureUnavailable", err)
	}
}
