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
	backoffFactor        = 3
	maxAttempts          = 3
	journalWriteTimeout  = 10 * time.Second
)

var (
	// progressInterval is a variable so tests can shorten it.
	progressInterval = 5 * time.Second
	// retryFirstBackoff is a variable so tests can avoid real waits.
	retryFirstBackoff = time.Second
	// errLimitReached stops enumeration when the user limit is reached.
	errLimitReached = errors.New("message limit reached")
)

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
	FolderID       string `json:"folder_id"`
	Path           string `json:"path"`
	Found          int    `json:"found"`
	Downloaded     int    `json:"downloaded"`
	Failed         int    `json:"failed"`
	Skipped        int    `json:"skipped"`
	ThreadsFailed  int    `json:"threads_failed"`
	ThreadsPartial int    `json:"threads_partial"`
	Partial        bool   `json:"partial"`
	Bytes          int64  `json:"bytes"`
}

// Summary reports the whole run.
type Summary struct {
	Found          int             `json:"found"`
	Downloaded     int             `json:"downloaded"`
	Failed         int             `json:"failed"`
	Skipped        int             `json:"skipped"`
	ThreadsFailed  int             `json:"threads_failed"`
	ThreadsPartial int             `json:"threads_partial"`
	FailedFolders  int             `json:"failed_folders"`
	Partial        bool            `json:"partial"`
	Bytes          int64           `json:"bytes"`
	PerFolder      []FolderSummary `json:"per_folder"`
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

	found          atomic.Int64
	downloaded     atomic.Int64
	failed         atomic.Int64
	skipped        atomic.Int64
	threadsFailed  atomic.Int64
	threadsPartial atomic.Int64
	failedFolders  atomic.Int64
	bytes          atomic.Int64
	attempted      atomic.Int64
	current        atomic.Value // string
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
		if fs.Found > 0 || fs.Skipped > 0 || fs.Downloaded > 0 || fs.Failed > 0 ||
			fs.Bytes > 0 || fs.ThreadsFailed > 0 || fs.ThreadsPartial > 0 || fs.Partial {
			out = append(out, fs)
		}
		if err != nil {
			if errors.Is(err, mailapi.ErrAuth) || errors.Is(err, context.Canceled) {
				return s.summary(out), err
			}
			s.failedFolders.Add(1)
			s.logf("folder %q: %v", node.Path, err)
			continue
		}
		if s.limitReached() {
			s.logf("limit of %d messages reached", s.opts.Limit)
			break
		}
	}
	// Cancellation during the last folder must not look like success.
	if err := ctx.Err(); err != nil {
		return s.summary(out), err
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

// runFolder exports one folder and returns its counters. Enumeration is
// streamed into the worker pool, so memory does not grow with the mailbox size.
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

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ch := make(chan job, s.opts.Workers*2)
	var wg sync.WaitGroup
	var mu sync.Mutex // Protected by mu: res, fatal.
	var res downloadResult
	var fatal error
	fail := func(err error) {
		mu.Lock()
		if fatal == nil {
			fatal = err
		}
		mu.Unlock()
		cancel()
	}
	if !s.opts.DryRun {
		for i := 0; i < s.opts.Workers; i++ {
			wg.Add(1)
			go func() { // Lifecycle: exits when ch closes or ctx is canceled; joined by wg.Wait.
				defer wg.Done()
				for j := range ch {
					s.worker(ctx, j, &res, &mu, fail)
				}
			}()
		}
	}

	pending := newPendingCache(s.opts.AccountDir, s.journal)
	stats, enumErr := s.enumerateFolder(ctx, node, func(j job) error {
		sum.Found++
		s.found.Add(1)
		if pending.exists(j) {
			sum.Skipped++
			s.skipped.Add(1)
			return nil
		}
		if s.opts.DryRun {
			return nil
		}
		if !s.reserve() {
			return errLimitReached
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- j:
			return nil
		}
	})
	close(ch) // Closed by producer (runFolder).
	wg.Wait()

	mu.Lock()
	sum.Downloaded, sum.Failed, sum.Bytes = res.downloaded, res.failed, res.bytes
	fatalCopy := fatal
	mu.Unlock()
	sum.ThreadsFailed, sum.ThreadsPartial = stats.ThreadsFailed, stats.ThreadsPartial
	s.threadsFailed.Add(int64(stats.ThreadsFailed))
	s.threadsPartial.Add(int64(stats.ThreadsPartial))
	if stats.Truncated {
		sum.Partial = true
		s.logf("folder %q: enumeration stopped before the end; rerun to continue", node.Path)
	}
	if fatalCopy != nil {
		return sum, fatalCopy
	}
	if enumErr != nil && !errors.Is(enumErr, errLimitReached) {
		sum.Partial = true
		return sum, enumErr
	}
	if errors.Is(enumErr, errLimitReached) {
		s.logf("limit of %d messages reached", s.opts.Limit)
	}
	if err := ctx.Err(); err != nil {
		return sum, err
	}
	return sum, nil
}

// reserve atomically claims one slot in the user limit.
func (s *Service) reserve() bool {
	if s.opts.Limit <= 0 {
		return true
	}
	for {
		cur := s.attempted.Load()
		if cur >= int64(s.opts.Limit) {
			return false
		}
		if s.attempted.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// pendingCache answers resume checks from directory listings instead of one
// os.Stat call per message.
type pendingCache struct {
	accountDir string
	journal    *journal.Journal
	dirs       map[string]map[string]int64
}

func newPendingCache(accountDir string, j *journal.Journal) *pendingCache {
	return &pendingCache{accountDir: accountDir, journal: j, dirs: map[string]map[string]int64{}}
}

func (p *pendingCache) exists(j job) bool {
	if p.journal == nil {
		return false
	}
	e, ok := p.journal.Status(j.messageID)
	if !ok || e.Status != journal.StatusDownloaded || e.Size <= 0 || e.Dest == "" {
		return false
	}
	dest := filepath.Join(p.accountDir, filepath.FromSlash(e.Dest))
	rel, err := filepath.Rel(p.accountDir, dest)
	if err != nil || !filepath.IsLocal(rel) {
		return false
	}
	dir := filepath.Dir(dest)
	sizes, ok := p.dirs[dir]
	if !ok {
		sizes = readDirSizes(dir)
		p.dirs[dir] = sizes
	}
	size, ok := sizes[filepath.Base(dest)]
	return ok && size == e.Size
}

func readDirSizes(dir string) map[string]int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	sizes := make(map[string]int64, len(entries))
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		info, err := ent.Info()
		if err == nil {
			sizes[ent.Name()] = info.Size()
		}
	}
	return sizes
}

// summary assembles counters for the final report.
func (s *Service) summary(perFolder []FolderSummary) Summary {
	sum := Summary{
		Found:          int(s.found.Load()),
		Downloaded:     int(s.downloaded.Load()),
		Failed:         int(s.failed.Load()),
		Skipped:        int(s.skipped.Load()),
		ThreadsFailed:  int(s.threadsFailed.Load()),
		ThreadsPartial: int(s.threadsPartial.Load()),
		FailedFolders:  int(s.failedFolders.Load()),
		Bytes:          s.bytes.Load(),
		PerFolder:      perFolder,
	}
	sum.Partial = sum.Failed > 0 || sum.ThreadsFailed > 0 || sum.ThreadsPartial > 0 || sum.FailedFolders > 0
	if !sum.Partial {
		for _, f := range perFolder {
			if f.Partial {
				sum.Partial = true
				break
			}
		}
	}
	return sum
}

// limitReached reports whether the run limit was reached.
func (s *Service) limitReached() bool {
	return s.opts.Limit > 0 && s.attempted.Load() >= int64(s.opts.Limit)
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
