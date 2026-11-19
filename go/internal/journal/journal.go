// Package journal is the crash-safe export log used for resume and reporting.
//
// REQ-EXPORT-003: every completed or failed message is appended to JSONL before
// the next download; a JSON snapshot is written periodically.
package journal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Message statuses.
const (
	StatusPending    = "pending"
	StatusDownloaded = "downloaded"
	StatusFailed     = "failed"
)

const (
	snapshotEvery = 2000
	flushSize     = 64 * 1024
	snapshotName  = "export-log.json"
	jsonlName     = "export-log.jsonl"
	closeTimeout  = 5 * time.Second
)

// Entry is one message record.
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

// Snapshot is the periodic full journal.
type Snapshot struct {
	Version   int     `json:"version"`
	Email     string  `json:"email"`
	StartedAt string  `json:"started_at"`
	UpdatedAt string  `json:"updated_at"`
	Stats     Stats   `json:"stats"`
	Entries   []Entry `json:"entries"`
}

// Stats aggregates journal entries.
type Stats struct {
	Total      int   `json:"total"`
	Downloaded int   `json:"downloaded"`
	Failed     int   `json:"failed"`
	Pending    int   `json:"pending"`
	Bytes      int64 `json:"bytes"`
}

// Journal owns the append-only log and the in-memory index.
type Journal struct {
	mu      sync.Mutex // Protected by mu.
	dir     string
	email   string
	started time.Time
	entries map[string]*Entry
	jsonl   *os.File
	writer  *bufio.Writer
	unsaved int
}

// Open loads an existing journal and opens the JSONL for appending.
func Open(dir, email string) (*Journal, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("REQ-EXPORT-003: create journal dir: %w", err)
	}
	j := &Journal{dir: dir, email: email, started: time.Now(), entries: map[string]*Entry{}}
	if err := j.load(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, jsonlName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("REQ-EXPORT-003: open jsonl: %w", err)
	}
	j.jsonl = f
	j.writer = bufio.NewWriterSize(f, flushSize)
	return j, nil
}

// load prefers JSONL because it is the crash-safe source of truth.
func (j *Journal) load() error {
	jsonlPath := filepath.Join(j.dir, jsonlName)
	if f, err := os.Open(jsonlPath); err == nil {
		// NOERR: read-only close.
		defer f.Close()
		return j.scan(f)
	}
	raw, err := os.ReadFile(filepath.Join(j.dir, snapshotName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("REQ-EXPORT-003: read snapshot: %w", err)
	}
	var snap Snapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: parse snapshot: %w", err)
	}
	for i := range snap.Entries {
		e := snap.Entries[i]
		if e.MessageID != "" {
			j.entries[e.MessageID] = &e
		}
	}
	return nil
}

// scan reads the append-only log into the index.
func (j *Journal) scan(f *os.File) error {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, flushSize), 4*1024*1024)
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.MessageID != "" {
			cp := e
			j.entries[e.MessageID] = &cp
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: scan jsonl: %w", err)
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

// Record stores an entry and appends it to the log.
func (j *Journal) Record(ctx context.Context, e Entry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	e.UpdatedAt = time.Now().Format(time.RFC3339)
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := e
	j.entries[e.MessageID] = &cp
	line, err := json.Marshal(&e)
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-003: encode entry: %w", err)
	}
	if _, err := j.writer.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: append jsonl: %w", err)
	}
	if err := j.writer.Flush(); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: flush jsonl: %w", err)
	}
	j.unsaved++
	if j.unsaved >= snapshotEvery {
		j.unsaved = 0
		return j.saveLocked(ctx)
	}
	return nil
}

// saveLocked writes a snapshot; the caller holds mu.
func (j *Journal) saveLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	raw, err := json.MarshalIndent(&snap, "", "  ")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-003: encode snapshot: %w", err)
	}
	tmp := filepath.Join(j.dir, snapshotName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: write snapshot: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(j.dir, snapshotName)); err != nil {
		return fmt.Errorf("REQ-EXPORT-003: replace snapshot: %w", err)
	}
	return nil
}

// Save writes a full snapshot.
func (j *Journal) Save(ctx context.Context) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.unsaved = 0
	return j.saveLocked(ctx)
}

// Stats recomputes totals from the in-memory index.
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

// Close writes the final snapshot, then flushes and closes the log.
// It always releases the file even when the snapshot fails.
func (j *Journal) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), closeTimeout)
	defer cancel()
	var errs []error
	if err := j.Save(ctx); err != nil {
		errs = append(errs, err)
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.writer != nil {
		if err := j.writer.Flush(); err != nil {
			errs = append(errs, fmt.Errorf("REQ-EXPORT-003: final flush: %w", err))
		}
	}
	if j.jsonl != nil {
		if err := j.jsonl.Close(); err != nil {
			errs = append(errs, fmt.Errorf("REQ-EXPORT-003: close jsonl: %w", err))
		}
	}
	return errors.Join(errs...)
}
