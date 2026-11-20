package mailapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ID is a folder identifier that may arrive as a JSON string or number.
type ID string

// String returns the identifier as a plain string.
func (id ID) String() string { return string(id) }

// UnmarshalJSON accepts both "42" and 42.
func (id *ID) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*id = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("folder id: %w", err)
		}
		*id = ID(s)
		return nil
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return fmt.Errorf("folder id: unsupported value %q", b)
		}
	}
	*id = ID(b)
	return nil
}

// Correspondent is one sender or recipient.
type Correspondent struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// Correspondents groups the from-list of a message.
type Correspondents struct {
	From []Correspondent `json:"from"`
}

// Sender returns the display name of the first sender, or its address.
func (c Correspondents) Sender() string {
	if len(c.From) == 0 {
		return ""
	}
	if c.From[0].Name != "" {
		return c.From[0].Name
	}
	return c.From[0].Email
}

// Folder is one mailbox folder.
type Folder struct {
	ID             ID     `json:"id"`
	Name           string `json:"name"`
	Parent         ID     `json:"parent"`
	MessagesTotal  int    `json:"messages_total"`
	MessagesUnread int    `json:"messages_unread"`
	System         bool   `json:"system"`
	Type           string `json:"type"`
}

// Thread is a row in the message list; it may contain several messages.
type Thread struct {
	ID             string         `json:"id"`
	Length         int            `json:"length"`
	Last           string         `json:"last"`
	Expand         *string        `json:"expand"`
	Subject        string         `json:"subject"`
	Date           int64          `json:"date"`
	Folder         ID             `json:"folder"`
	Correspondents Correspondents `json:"correspondents"`
}

// Message is one letter inside a thread.
type Message struct {
	ID             string         `json:"id"`
	Subject        string         `json:"subject"`
	Date           int64          `json:"date"`
	Folder         ID             `json:"folder"`
	Correspondents Correspondents `json:"correspondents"`
}

// Pinned holds messages pinned at the top of a folder.
type Pinned struct {
	Messages []Thread `json:"messages"`
}

// ListBody is the body of threads/status/smart.
type ListBody struct {
	Folders      []Folder `json:"folders"`
	Threads      []Thread `json:"threads"`
	Pinned       Pinned   `json:"pinned"`
	ThreadsTotal int      `json:"threads_total"`
}

// Order selects the thread sort direction.
type Order string

const (
	// OrderAsc sorts oldest first, so appends do not shift read offsets.
	OrderAsc Order = "asc"
	// OrderDesc sorts newest first.
	OrderDesc Order = "desc"
)

// flexString accepts both strings and numbers in JSON.
type flexString string

// UnmarshalJSON implements json.Unmarshaler for flexString.
func (f *flexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		*f = ""
		return nil
	}
	if b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	*f = flexString(strings.TrimSpace(string(b)))
	return nil
}
