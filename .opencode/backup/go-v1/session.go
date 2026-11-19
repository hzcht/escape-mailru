package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type Cookie struct {
	Name    string  `json:"name"`
	Value   string  `json:"value"`
	Domain  string  `json:"domain"`
	Path    string  `json:"path"`
	Expires float64 `json:"expires"`
}

type Session struct {
	Email       string   `json:"email"`
	AccessToken string   `json:"access_token"`
	CSRFToken   string   `json:"csrf_token"`
	Cookies     []Cookie `json:"cookies"`
}

func LoadSession(path string) (Session, error) {
	var s Session
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, fmt.Errorf("read session: %w", err)
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("parse session: %w", err)
	}
	if s.Email == "" {
		return s, fmt.Errorf("session: email is empty")
	}
	if s.AccessToken == "" {
		return s, fmt.Errorf("session: access_token is empty")
	}
	if len(s.Cookies) == 0 {
		return s, fmt.Errorf("session: cookies are empty")
	}
	return s, nil
}

func (s Session) CookieHeader() string {
	now := float64(time.Now().Unix())
	parts := make([]string, 0, len(s.Cookies))
	for _, c := range s.Cookies {
		if !strings.Contains(c.Domain, "mail.ru") {
			continue
		}
		if c.Expires > 0 && c.Expires < now {
			continue
		}
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
