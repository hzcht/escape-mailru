package session

import (
	"os"
	"path/filepath"
	"strings"
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
		{Name: "upper", Value: "6", Domain: ".MAIL.RU"},
		{Name: "evil", Value: "7", Domain: "evilmail.ru"},
		{Name: "suffix", Value: "8", Domain: "mail.ru.evil.com"},
	}}
	got := s.CookieHeader(now)
	want := "s=1; future=3; session=5; upper=6"
	if got != want {
		t.Fatalf("cookie header = %q, want %q", got, want)
	}
}

func TestCookieHeaderVKDomains(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := Session{Cookies: []Cookie{
		{Name: "mail", Value: "m", Domain: ".mail.ru"},
		{Name: "vk", Value: "v", Domain: ".vk.com"},
		{Name: "workspace", Value: "w", Domain: "workspace.vk.ru"},
		{Name: "userapi", Value: "u", Domain: ".userapi.com"},
		{Name: "evil", Value: "x", Domain: "vk.com.evil.example"},
	}}
	got := s.CookieHeader(now)
	want := "mail=m; vk=v; workspace=w; userapi=u"
	if got != want {
		t.Fatalf("cookie header = %q, want %q", got, want)
	}
}

func TestValidateRequiresUsableCookies(t *testing.T) {
	now := time.Now()
	s := Session{
		Email:       "user@mail_ru_hosted_domain",
		AccessToken: "t",
		Cookies: []Cookie{
			{Name: "s", Value: "1", Domain: ".mail.ru", Expires: float64(now.Add(-time.Hour).Unix())},
		},
	}
	if err := Validate(s); err == nil {
		t.Fatal("expired cookie must fail validation")
	}
}

func TestExpiryWarning(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := Session{Cookies: []Cookie{
		{Name: "soon", Value: "x", Domain: ".mail.ru", Expires: float64(now.Add(48 * time.Hour).Unix())},
		{Name: "later", Value: "y", Domain: ".mail.ru", Expires: float64(now.Add(30 * 24 * time.Hour).Unix())},
		{Name: "session", Value: "z", Domain: ".mail.ru"},
	}}
	warning := ExpiryWarning(s, now)
	if !strings.Contains(warning, "soon") || strings.Contains(warning, "later") || strings.Contains(warning, "session") {
		t.Fatalf("warning = %q", warning)
	}
}

func TestValidate(t *testing.T) {
	if err := Validate(Session{}); err == nil {
		t.Fatal("expected error for empty session")
	}
	if err := Validate(Session{Email: "u", AccessToken: "t", Cookies: []Cookie{{Name: "s"}}}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func validSession() Session {
	return Session{
		Email:       "user@example.com",
		AccessToken: "t",
		CSRFToken:   "c",
		Cookies:     []Cookie{{Name: "s", Value: "1", Domain: ".mail.ru"}},
	}
}

func TestSaveAndLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "session.json")
	in := validSession()
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Email != in.Email || out.AccessToken != in.AccessToken || out.CSRFToken != in.CSRFToken || len(out.Cookies) != 1 {
		t.Fatalf("loaded = %+v", out)
	}
}

func TestSaveRejectsInvalidSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := Save(path, Session{}); err == nil {
		t.Fatal("expected validation error")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("invalid session was written: %v", err)
	}
}

func TestSaveBadParent(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The parent of the target is a regular file, so MkdirAll must fail.
	if err := Save(filepath.Join(blocker, "session.json"), validSession()); err == nil {
		t.Fatal("expected directory creation error")
	}
}

func TestSaveReplacesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	first := validSession()
	first.Cookies[0].Value = "old"
	if err := Save(path, first); err != nil {
		t.Fatal(err)
	}
	second := validSession()
	second.Cookies[0].Value = "new"
	if err := Save(path, second); err != nil {
		t.Fatal(err)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Cookies[0].Value != "new" {
		t.Fatalf("cookie = %q, want new", out.Cookies[0].Value)
	}
}

func TestSaveRenameError(t *testing.T) {
	dir := t.TempDir()
	// The destination is an existing directory, so the final rename fails.
	dst := filepath.Join(dir, "session.json")
	if err := os.MkdirAll(filepath.Join(dst, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Save(dst, validSession()); err == nil {
		t.Fatal("expected rename error")
	}
}
