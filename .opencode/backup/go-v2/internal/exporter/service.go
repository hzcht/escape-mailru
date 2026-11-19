// Package exporter enumerates mailbox folders and stores original .eml files.
//
// REQ-EXPORT-001: every selected folder is enumerated in ascending date order.
// REQ-EXPORT-002: downloaded bytes are verified and written atomically.
// REQ-EXPORT-003: the journal is updated before the next message is processed.
package exporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"mexporter/internal/journal"
	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

const (
	pagePause            = 120 * time.Millisecond
	maxEnumerationOffset = 2_000_000
	maxStalePages        = 2
	firstBackoff         = time.Second
	backoffFactor        = 3
	maxAttempts          = 3
	rateLimitPause       = 30 * time.Second
	journalWriteTimeout  = 10 * time.Second
)

// progressInterval is a variable so tests can shorten it.
var progressInterval = 5 * time.Second

// API is the subset of mailapi.Client used by the exporter.
type API interface {
	ListPage(ctx context.Context, folderID string, offset, limit int, order mailapi.Order) (*mailapi.ListBody, error)
	ThreadMessages(ctx context.Context, threadID string) ([]mailapi.Message, error)
	DownloadRaw(ctx context.Context, messageID, folderID string) ([]byte, error)
}

// Options controls one export run.
type Options struct {
	AccountDir string
	Folders    []string
	Workers    int
	Delay      time.Duration
	Limit      int
	PageSize   int
	DryRun     bool
	Verbose    bool
	Progress   io.Writer
	Logf       func(format string, args ...any)
}

// FolderSummary reports one folder after a run.
type FolderSummary struct {
	FolderID   string
	Path       string
	Found      int
	Downloaded int
	Failed     int
	Skipped    int
	Bytes      int64
}

// Summary reports the whole run.
type Summary struct {
	Found      int
	Downloaded int
	Failed     int
	Skipped    int
	Bytes      int64
	PerFolder  []FolderSummary
}

type job struct {
	messageID  string
	folderID   string
	folderPath string
	subject    string
	sender     string
	dateUnix   int64
}

// Service is a single-run exporter.
type Service struct {
	api     API
	journal *journal.Journal
	opts    Options
	seen    map[string]struct{}

	found           atomic.Int64
	downloaded      atomic.Int64
	failed          atomic.Int64
	skipped         atomic.Int64
	bytes           atomic.Int64
	totalDownloaded atomic.Int64
	current         atomic.Value // string
}

// New builds a service. Zero workers/page size fall back to safe defaults.
func New(api API, j *journal.Journal, opts Options) *Service {
	if opts.Workers <= 0 {
		opts.Workers = 1
	}
	if opts.PageSize <= 0 {
		opts.PageSize = 50
	}
	return &Service{api: api, journal: j, opts: opts, seen: map[string]struct{}{}}
}

// Run exports the selected folders in tree order.
func (s *Service) Run(ctx context.Context, nodes []*layout.Node) (Summary, error) {
	stop := make(chan struct{})
	var progressWG sync.WaitGroup
	if s.opts.Progress != nil {
		progressWG.Add(1)
		go func() { // Lifecycle: exits when stop closes or ctx ends; joined by progressWG.Wait.
			defer progressWG.Done()
			s.progressLoop(ctx, stop)
		}()
	}
	// Closed by Run after the progress goroutine is created.
	defer func() { close(stop); progressWG.Wait() }()

	selected := s.selectedSet()
	var out []FolderSummary
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return s.summary(out), err
		}
		if len(selected) > 0 && !selected[node.ID.String()] {
			continue
		}
		fs, err := s.runFolder(ctx, node)
		if fs.Found > 0 || fs.Skipped > 0 || fs.Downloaded > 0 || fs.Failed > 0 || fs.Bytes > 0 {
			out = append(out, fs)
		}
		if err != nil {
			if errors.Is(err, mailapi.ErrAuth) || errors.Is(err, context.Canceled) {
				return s.summary(out), err
			}
			s.logf("folder %q: %v", node.Path, err)
			continue
		}
		if s.limitReached() {
			s.logf("limit of %d messages reached", s.opts.Limit)
			break
		}
	}
	return s.summary(out), nil
}

// selectedSet returns nil when every folder is selected.
func (s *Service) selectedSet() map[string]bool {
	if len(s.opts.Folders) == 0 {
		return nil
	}
	set := make(map[string]bool, len(s.opts.Folders))
	for _, id := range s.opts.Folders {
		if id != "" {
			set[id] = true
		}
	}
	return set
}

// runFolder exports one folder and returns its counters.
func (s *Service) runFolder(ctx context.Context, node *layout.Node) (FolderSummary, error) {
	sum := FolderSummary{FolderID: node.ID.String(), Path: node.Path}
	destDir := filepath.Join(s.opts.AccountDir, filepath.FromSlash(node.Path))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return sum, fmt.Errorf("REQ-EXPORT-001: create folder dir: %w", err)
	}
	if node.MessagesTotal == 0 {
		return sum, nil
	}
	s.current.Store(node.Path)
	s.logf("folder %q: enumerating %d messages", node.Path, node.MessagesTotal)
	jobs, err := s.enumerateFolder(ctx, node)
	if err != nil {
		return sum, err
	}
	sum.Found = len(jobs)
	s.found.Add(int64(len(jobs)))
	pending := s.pendingJobs(jobs)
	sum.Skipped = len(jobs) - len(pending)
	s.skipped.Add(int64(sum.Skipped))
	s.logf("folder %q: %d to download, %d already present", node.Path, len(pending), sum.Skipped)
	if s.opts.DryRun || len(pending) == 0 {
		return sum, nil
	}
	res, err := s.downloadFolder(ctx, node, pending)
	sum.Downloaded, sum.Failed, sum.Bytes = res.downloaded, res.failed, res.bytes
	return sum, err
}

// pendingJobs drops messages that are already verified on disk.
func (s *Service) pendingJobs(jobs []job) []job {
	pending := make([]job, 0, len(jobs))
	for _, j := range jobs {
		e, ok := s.journal.Status(j.messageID)
		if ok && e.Status == journal.StatusDownloaded {
			st, err := os.Stat(filepath.Join(s.opts.AccountDir, filepath.FromSlash(e.Dest)))
			if err == nil && st.Size() > 0 {
				continue
			}
		}
		pending = append(pending, j)
	}
	return pending
}

// summary assembles counters for the final report.
func (s *Service) summary(perFolder []FolderSummary) Summary {
	return Summary{
		Found:      int(s.found.Load()),
		Downloaded: int(s.downloaded.Load()),
		Failed:     int(s.failed.Load()),
		Skipped:    int(s.skipped.Load()),
		Bytes:      s.bytes.Load(),
		PerFolder:  perFolder,
	}
}

// limitReached reports whether the run limit was reached.
func (s *Service) limitReached() bool {
	return s.opts.Limit > 0 && s.totalDownloaded.Load() >= int64(s.opts.Limit)
}

// logf writes a diagnostic line when a logger is configured.
func (s *Service) logf(format string, args ...any) {
	if s.opts.Logf != nil {
		s.opts.Logf(format, args...)
	}
}

// progressLoop prints throughput every few seconds.
func (s *Service) progressLoop(ctx context.Context, stop <-chan struct{}) {
	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()
	var lastBytes int64
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case now := <-ticker.C:
			b := s.bytes.Load()
			speed := float64(b-lastBytes) / 1024 / now.Sub(last).Seconds()
			lastBytes, last = b, now
			cur, _ := s.current.Load().(string)
			fmt.Fprintf(s.opts.Progress, "\r[%s] %s | ok=%d fail=%d skip=%d | %.1f MB | %.0f KB/s   ",
				now.Format("15:04:05"), truncateRunes(cur, 40), s.downloaded.Load(), s.failed.Load(),
				s.skipped.Load(), float64(b)/1024/1024, speed)
		}
	}
}

// truncateRunes shortens a string without splitting runes.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	return string(r[:n-1]) + "…"
}
