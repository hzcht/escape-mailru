package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeSession(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	path := writeSession(t, `{"email":"user@mail_ru_hosted_domain","access_token":"t","cookies":[{"name":"s","value":"1","domain":".mail.ru"}]}`)
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "user@mail_ru_hosted_domain" || s.AccessToken != "t" || len(s.Cookies) != 1 {
		t.Fatalf("session = %+v", s)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"missing", ""},
		{"bad json", "{"},
		{"empty email", `{"access_token":"t","cookies":[{"name":"s"}]}`},
		{"empty token", `{"email":"e","cookies":[{"name":"s"}]}`},
		{"empty cookies", `{"email":"e","access_token":"t"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var path string
			if tt.name == "missing" {
				path = filepath.Join(t.TempDir(), "absent.json")
			} else {
				path = writeSession(t, tt.content)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestCookieHeader(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := Session{Cookies: []Cookie{
		{Name: "s", Value: "1", Domain: ".mail.ru"},
		{Name: "expired", Value: "2", Domain: "e.mail.ru", Expires: float64(now.Unix() - 1)},
		{Name: "future", Value: "3", Domain: "e.mail.ru", Expires: float64(now.Unix() + 60)},
		{Name: "other", Value: "4", Domain: ".example.com"},
		{Name: "session", Value: "5", Domain: "mail.ru"},
	}}
	got := s.CookieHeader(now)
	want := "s=1; future=3; session=5"
	if got != want {
		t.Fatalf("cookie header = %q, want %q", got, want)
	}
}
