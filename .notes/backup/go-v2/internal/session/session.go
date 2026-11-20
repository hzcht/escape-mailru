// Package session loads the browser session captured from the VK WorkSpace UI.
//
// REQ-EXPORT-010: credentials are read only from the session file and are never logged.
package session

import (
	"encoding/json"
	"fmt"
	"os"
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
	switch {
	case s.Email == "":
		return s, fmt.Errorf("REQ-EXPORT-010: session email is empty")
	case s.AccessToken == "":
		return s, fmt.Errorf("REQ-EXPORT-010: session access_token is empty")
	case len(s.Cookies) == 0:
		return s, fmt.Errorf("REQ-EXPORT-010: session cookies are empty")
	}
	return s, nil
}

// CookieHeader renders the Cookie request header at the given time.
func (s Session) CookieHeader(now time.Time) string {
	unix := float64(now.Unix())
	parts := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		if !strings.Contains(c.Domain, "mail.ru") {
			continue
		}
		if c.Expires > 0 && c.Expires < unix {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
