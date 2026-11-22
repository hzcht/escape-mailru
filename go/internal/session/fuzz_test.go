package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzSessionJSON feeds arbitrary bytes to the session parser. Whatever comes
// out, Validate and CookieHeader must stay consistent and must never panic.
func FuzzSessionJSON(f *testing.F) {
	f.Add([]byte(`{"email":"a@b.c","access_token":"t","cookies":[]}`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"email":"a@b.c","access_token":"t","cookies":[{"name":"m","value":"v","domain":"mail.ru"}]}`))
	f.Add([]byte(`{"cookies":[{"name":"m","value":"v","domain":"evil.example","expires":9999999999}]}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s Session
		if err := json.Unmarshal(data, &s); err != nil {
			return // malformed JSON is rejected before any validation.
		}
		header := s.CookieHeader(time.Now())
		if err := Validate(s, time.Now()); err == nil && header == "" {
			t.Fatalf("Validate accepted a session with no usable cookies: %q", string(data))
		}
		// Expiry reporting must never leak cookie values.
		if warn := ExpiryWarning(s, time.Now()); warn != "" {
			for _, c := range s.Cookies {
				if c.Value != "" && strings.Contains(warn, c.Value) {
					t.Fatalf("expiry warning leaks a cookie value: %q", warn)
				}
			}
		}
	})
}

// FuzzCookieHeaderDomains checks the cookie allow-list: rejected domains and
// expired cookies never reach the request header, accepted ones always do.
func FuzzCookieHeaderDomains(f *testing.F) {
	f.Add("sid", "secret", ".mail.ru", float64(4102444800))
	f.Add("sid", "secret", "vk.com.evil.example", float64(4102444800))
	f.Add("sid", "secret", "workspace.vk.ru", float64(4102444800))
	f.Add("sid", "secret", "mail.ru", float64(1))
	f.Add("sid", "secret", "", float64(4102444800))
	f.Fuzz(func(t *testing.T, name, value, domain string, expires float64) {
		s := Session{
			Email:       "user@example.com",
			AccessToken: "token",
			Cookies:     []Cookie{{Name: name, Value: value, Domain: domain, Expires: expires}},
		}
		now := time.Unix(1700000000, 0)
		header := s.CookieHeader(now)

		if !AcceptsDomain(domain) {
			if header != "" {
				t.Fatalf("rejected domain %q leaked into %q", domain, header)
			}
			return
		}
		if expires > 0 && expires < float64(now.Unix()) {
			if header != "" {
				t.Fatalf("expired cookie for %q leaked into %q", domain, header)
			}
			return
		}
		if header != "" && !strings.Contains(header, name+"="+value) {
			t.Fatalf("accepted cookie missing from %q", header)
		}
	})
}
