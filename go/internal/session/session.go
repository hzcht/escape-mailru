// Package session loads the browser session captured from the VK WorkSpace UI.
//
// REQ-EXPORT-010: credentials are read only from the session file and are never logged.
package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Cookie is one browser cookie from the exported session.
type Cookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

// Session holds the minimum state required to call the mail API.
//
// Token is the web-session token the current mail UI passes as "token" next
// to the cookies; AccessToken is the older query credential. A session carries
// whichever one the account uses.
type Session struct {
	Email       string   `json:"email"`
	AccessToken string   `json:"access_token"`
	Token       string   `json:"token"`
	CSRFToken   string   `json:"csrf_token"`
	Cookies     []Cookie `json:"cookies"`
}

// Load reads and validates a session file.
//
// REQ-EXPORT-010:
//   - Pre-conditions: path points to a JSON object exported from the browser.
//   - Post-conditions: returned session has email, a token (web or legacy) and at least one cookie.
//   - Errors: wraps filesystem, JSON and validation failures.
func Load(path string) (Session, error) {
	var s Session
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("REQ-EXPORT-010: read session: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("REQ-EXPORT-010: parse session: %w", err)
	}
	if err := Validate(s, time.Now()); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Validate reports whether the session has the minimum required fields.
// The current time is passed in so validation is deterministic in tests.
func Validate(s Session, now time.Time) error {
	switch {
	case s.Email == "":
		return fmt.Errorf("REQ-EXPORT-010: session email is empty")
	case s.AccessToken == "" && s.Token == "":
		return fmt.Errorf("REQ-EXPORT-010: session has neither access_token nor token")
	case len(s.Cookies) == 0:
		return fmt.Errorf("REQ-EXPORT-010: session cookies are empty")
	case s.CookieHeader(now) == "":
		return fmt.Errorf("REQ-EXPORT-010: session has no usable, unexpired cookies")
	}
	return nil
}

// Save writes the session atomically with restrictive permissions.
//
// REQ-EXPORT-010: the session contains secrets and is only stored at the requested path.
func Save(path string, s Session) error {
	if err := Validate(s, time.Now()); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("REQ-EXPORT-010: create session dir: %w", err)
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-010: encode session: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.tmp")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-010: create session temp: %w", err)
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-010: chmod session temp: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-010: write session: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-010: close session: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-010: replace session: %w", err)
	}
	return nil
}

// CookieHeader renders the Cookie request header at the given time.
func (s Session) CookieHeader(now time.Time) string {
	unix := float64(now.Unix())
	parts := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		if !AcceptsDomain(c.Domain) {
			continue
		}
		if c.Expires > 0 && c.Expires < unix {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ExpiryWarning reports cookies from mail domains that expire within seven
// days without exposing values. Already expired and unrelated cookies are not
// reported: they cannot be used for the API anyway.
func ExpiryWarning(s Session, now time.Time) string {
	unix := float64(now.Unix())
	deadline := now.Add(7 * 24 * time.Hour).Unix()
	names := make([]string, 0, 4)
	for _, c := range s.Cookies {
		if !AcceptsDomain(c.Domain) {
			continue
		}
		if c.Expires < unix || int64(c.Expires) > deadline {
			continue
		}
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%d cookie(s) expire within 7 days: %s", len(names), strings.Join(names, ", "))
}

// EarliestExpiry returns the soonest cookie expiry among cookies that are sent
// to the mail API. The boolean is false when no cookie expires.
func EarliestExpiry(s Session) (time.Time, bool) {
	var earliest time.Time
	for _, c := range s.Cookies {
		if c.Expires <= 0 || !AcceptsDomain(c.Domain) {
			continue
		}
		at := time.Unix(int64(c.Expires), 0)
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}

// AcceptsDomain reports whether a cookie from domain is forwarded to the mail
// API. It is the single allow-list used when reading a session file and when
// capturing cookies straight from the browser.
func AcceptsDomain(domain string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	if d == "" {
		return true // Host-only cookie from a browser export.
	}
	for _, base := range []string{"mail.ru", "vk.com", "vk.ru", "userapi.com"} {
		if d == base || strings.HasSuffix(d, "."+base) {
			return true
		}
	}
	return false
}
