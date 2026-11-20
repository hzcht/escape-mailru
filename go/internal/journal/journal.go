// Package journal is the crash-safe export log used for resume and reporting.
//
// REQ-EXPORT-003: every completed or failed message is appended to JSONL before
// the next download; a JSON snapshot is written periodically.
package journal

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
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
	SHA256    string `json:"sha256,omitempty"`
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

// OpenReadOnly loads the journal without creating or modifying files.
func OpenReadOnly(dir, email string) (*Journal, error) {
	j := &Journal{dir: dir, email: email, started: time.Now(), entries: map[string]*Entry{}}
	if err := j.load(); err != nil {
		return nil, err
	}
	return j, nil
}

// load restores the snapshot first, then replays JSONL over it. JSONL is the
// crash-safe source of truth, but an empty or truncated JSONL must not erase
// entries that are already in the snapshot.
func (j *Journal) load() error {
	snapErr := j.loadSnapshot()
	f, err := os.Open(filepath.Join(j.dir, jsonlName))
	if err != nil {
		if os.IsNotExist(err) {
			return snapErr
		}
		return fmt.Errorf("REQ-EXPORT-003: open jsonl: %w", err)
	}
	defer f.Close() // NOERR: read-only close.
	n, err := j.scan(f)
	if err != nil {
		return err
	}
	if n == 0 {
		// A snapshot error is fatal only when JSONL has nothing to replay.
		return snapErr
	}
	return nil
}

func (j *Journal) loadSnapshot() error {
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

// scan reads the append-only log into the index and returns the number of
// valid entries it saw.
func (j *Journal) scan(f *os.File) (int, error) {
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, flushSize), 4*1024*1024)
	n := 0
	for sc.Scan() {
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.MessageID != "" {
			cp := e
			j.entries[e.MessageID] = &cp
			n++
		}
	}
	if err := sc.Err(); err != nil {
		return n, fmt.Errorf("REQ-EXPORT-003: scan jsonl: %w", err)
	}
	return n, nil
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
	}
	sort.Slice(snap.Entries, func(a, b int) bool {
		return snap.Entries[a].MessageID < snap.Entries[b].MessageID
	})
	for i := range snap.Entries {
		e := &snap.Entries[i]
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

// Entries returns a deterministic copy of the in-memory index.
func (j *Journal) Entries() []Entry {
	j.mu.Lock()
	defer j.mu.Unlock()
	out := make([]Entry, 0, len(j.entries))
	for _, e := range j.entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(a, b int) bool {
		return out[a].MessageID < out[b].MessageID
	})
	return out
}

// VerifyIssue describes one downloaded entry that does not match its file.
type VerifyIssue struct {
	MessageID string
	Dest      string
	Kind      string
	Detail    string
}

// VerifyReport summarizes an offline check of downloaded journal entries.
type VerifyReport struct {
	Total        int
	OK           int
	Missing      int
	SizeMismatch int
	HashMismatch int
	NoHash       int
	Unsafe       int
	Unreadable   int
	Issues       []VerifyIssue
	Truncated    bool
}

const verifyIssueLimit = 100

// Verify checks downloaded entries against files below accountDir. With full
// set, it also compares SHA-256 for entries written by newer versions.
func (j *Journal) Verify(accountDir string, full bool) (VerifyReport, error) {
	abs, err := filepath.Abs(accountDir)
	if err != nil {
		return VerifyReport{}, fmt.Errorf("REQ-EXPORT-003: resolve account dir: %w", err)
	}
	var rep VerifyReport
	for _, e := range j.Entries() {
		if e.Status != StatusDownloaded {
			continue
		}
		rep.Total++
		bad := false
		addIssue := func(kind, detail string) {
			if len(rep.Issues) >= verifyIssueLimit {
				rep.Truncated = true
				return
			}
			rep.Issues = append(rep.Issues, VerifyIssue{
				MessageID: e.MessageID,
				Dest:      e.Dest,
				Kind:      kind,
				Detail:    detail,
			})
		}
		dest := strings.TrimSpace(e.Dest)
		if dest == "" {
			rep.Missing++
			addIssue("missing-path", "")
			continue
		}
		clean := filepath.Clean(filepath.FromSlash(dest))
		if filepath.IsAbs(clean) || filepath.VolumeName(clean) != "" ||
			clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			rep.Unsafe++
			addIssue("unsafe-path", dest)
			continue
		}
		path := filepath.Join(abs, clean)
		rel, relErr := filepath.Rel(abs, path)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			rep.Unsafe++
			addIssue("unsafe-path", dest)
			continue
		}
		st, statErr := os.Stat(path)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				rep.Missing++
			} else {
				rep.Unreadable++
			}
			addIssue("stat", statErr.Error())
			continue
		}
		if st.IsDir() {
			rep.Unreadable++
			addIssue("not-file", dest)
			continue
		}
		if e.Size > 0 && st.Size() != e.Size {
			bad = true
			rep.SizeMismatch++
			addIssue("size", fmt.Sprintf("file=%d journal=%d", st.Size(), e.Size))
		} else if e.Size == 0 && st.Size() == 0 {
			bad = true
			rep.SizeMismatch++
			addIssue("empty", "")
		}
		if full {
			if e.SHA256 == "" {
				rep.NoHash++
			} else {
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					bad = true
					rep.Unreadable++
					addIssue("read", readErr.Error())
				} else {
					sum := sha256.Sum256(raw)
					got := hex.EncodeToString(sum[:])
					if !strings.EqualFold(got, e.SHA256) {
						bad = true
						rep.HashMismatch++
						addIssue("hash", fmt.Sprintf("file=%s journal=%s", got, e.SHA256))
					}
				}
			}
		}
		if !bad {
			rep.OK++
		}
	}
	return rep, nil
}

// Close writes the final snapshot, then flushes and closes the log.
// It always releases the file even when the snapshot fails.
func (j *Journal) Close() error {
	if j.jsonl == nil {
		return nil
	}
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
