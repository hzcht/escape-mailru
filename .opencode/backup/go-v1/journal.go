package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	StatusPending    = "pending"
	StatusDownloaded = "downloaded"
	StatusFailed     = "failed"
)

type Entry struct {
	Folder    string `json:"folder"`
	FolderID  string `json:"folder_id"`
	MessageID string `json:"id"`
	Date      string `json:"date"`
	Subject   string `json:"subject"`
	Sender    string `json:"sender"`
	Dest      string `json:"dest_file"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
	Size      int64  `json:"size,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

type Snapshot struct {
	Version   int     `json:"version"`
	Email     string  `json:"email"`
	StartedAt string  `json:"started_at"`
	UpdatedAt string  `json:"updated_at"`
	Stats     Stats   `json:"stats"`
	Entries   []Entry `json:"entries"`
}

type Stats struct {
	Total      int   `json:"total"`
	Downloaded int   `json:"downloaded"`
	Failed     int   `json:"failed"`
	Pending    int   `json:"pending"`
	Bytes      int64 `json:"bytes"`
}

type Journal struct {
	mu      sync.Mutex
	dir     string
	email   string
	started time.Time
	entries map[string]*Entry
	jsonl   *os.File
	writer  *bufio.Writer
	unsaved int
	stats   Stats
}

func OpenJournal(dir, email string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j := &Journal{
		dir:     dir,
		email:   email,
		started: time.Now(),
		entries: map[string]*Entry{},
	}
	if err := j.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "export-log.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	j.jsonl = f
	j.writer = bufio.NewWriterSize(f, 64*1024)
	return j, nil
}

func (j *Journal) load() error {
	jsonlPath := filepath.Join(j.dir, "export-log.jsonl")
	if f, err := os.Open(jsonlPath); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var e Entry
			if json.Unmarshal(line, &e) == nil && e.MessageID != "" {
				cp := e
				j.entries[e.MessageID] = &cp
			}
		}
		return sc.Err()
	}
	jsonPath := filepath.Join(j.dir, "export-log.json")
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return err
	}
	for i := range snap.Entries {
		e := snap.Entries[i]
		if e.MessageID != "" {
			j.entries[e.MessageID] = &e
		}
	}
	return nil
}

// Status returns the stored entry for a message.
func (j *Journal) Status(messageID string) (Entry, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	e, ok := j.entries[messageID]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// Record stores an entry and appends it to the crash-safe log.
func (j *Journal) Record(e Entry) error {
	e.UpdatedAt = time.Now().Format(time.RFC3339)
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := e
	j.entries[e.MessageID] = &cp
	line, err := json.Marshal(&e)
	if err != nil {
		return err
	}
	if _, err := j.writer.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := j.writer.Flush(); err != nil {
		return err
	}
	j.unsaved++
	if j.unsaved >= 2000 {
		j.unsaved = 0
		return j.saveLocked()
	}
	return nil
}

func (j *Journal) saveLocked() error {
	snap := Snapshot{
		Version:   1,
		Email:     j.email,
		StartedAt: j.started.Format(time.RFC3339),
		UpdatedAt: time.Now().Format(time.RFC3339),
		Entries:   make([]Entry, 0, len(j.entries)),
	}
	for _, e := range j.entries {
		snap.Entries = append(snap.Entries, *e)
		switch e.Status {
		case StatusDownloaded:
			snap.Stats.Downloaded++
			snap.Stats.Bytes += e.Size
		case StatusFailed:
			snap.Stats.Failed++
		default:
			snap.Stats.Pending++
		}
	}
	snap.Stats.Total = len(snap.Entries)
	j.stats = snap.Stats

	raw, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(j.dir, "export-log.json.tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(j.dir, "export-log.json"))
}

// Save writes a full snapshot.
func (j *Journal) Save() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.unsaved = 0
	return j.saveLocked()
}

func (j *Journal) Stats() Stats {
	j.mu.Lock()
	defer j.mu.Unlock()
	var s Stats
	for _, e := range j.entries {
		s.Total++
		switch e.Status {
		case StatusDownloaded:
			s.Downloaded++
			s.Bytes += e.Size
		case StatusFailed:
			s.Failed++
		default:
			s.Pending++
		}
	}
	return s
}

func (j *Journal) Close() error {
	if err := j.Save(); err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writer != nil {
		if err := j.writer.Flush(); err != nil {
			return err
		}
	}
	if j.jsonl != nil {
		return j.jsonl.Close()
	}
	return nil
}

func (j *Journal) String() string {
	s := j.Stats()
	return fmt.Sprintf("total=%d downloaded=%d failed=%d pending=%d bytes=%d", s.Total, s.Downloaded, s.Failed, s.Pending, s.Bytes)
}
