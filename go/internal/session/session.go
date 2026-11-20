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
type Session struct {
	Email       string   `json:"email"`
	AccessToken string   `json:"access_token"`
	CSRFToken   string   `json:"csrf_token"`
	Cookies     []Cookie `json:"cookies"`
}

// Load reads and validates a session file.
//
// REQ-EXPORT-010:
//   - Pre-conditions: path points to a JSON object exported from the browser.
//   - Post-conditions: returned session has email, access_token and at least one cookie.
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
	if err := Validate(s); err != nil {
		return Session{}, err
	}
	return s, nil
}

// Validate reports whether the session has the minimum required fields.
func Validate(s Session) error {
	switch {
	case s.Email == "":
		return fmt.Errorf("REQ-EXPORT-010: session email is empty")
	case s.AccessToken == "":
		return fmt.Errorf("REQ-EXPORT-010: session access_token is empty")
	case len(s.Cookies) == 0:
		return fmt.Errorf("REQ-EXPORT-010: session cookies are empty")
	case s.CookieHeader(time.Now()) == "":
		return fmt.Errorf("REQ-EXPORT-010: session has no usable, unexpired cookies")
	}
	return nil
}

// Save writes the session atomically with restrictive permissions.
//
// REQ-EXPORT-010: the session contains secrets and is only stored at the requested path.
func Save(path string, s Session) error {
	if err := Validate(s); err != nil {
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
		if !mailDomain(c.Domain) {
			continue
		}
		if c.Expires > 0 && c.Expires < unix {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// ExpiryWarning reports cookies that expire soon without exposing values.
func ExpiryWarning(s Session, now time.Time) string {
	deadline := now.Add(7 * 24 * time.Hour).Unix()
	names := make([]string, 0, 4)
	for _, c := range s.Cookies {
		if c.Expires > 0 && int64(c.Expires) <= deadline {
			names = append(names, c.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return fmt.Sprintf("%d cookie(s) expire within 7 days: %s", len(names), strings.Join(names, ", "))
}

// mailDomain reports whether the cookie belongs to a supported mail/VK host.
func mailDomain(domain string) bool {
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
