package exporter

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mexporter/internal/journal"
	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

type downloadResult struct {
	downloaded int
	failed     int
	bytes      int64
}

// downloadFolder runs a worker pool over pending jobs.
func (s *Service) downloadFolder(ctx context.Context, node *layout.Node, jobs []job) (downloadResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var res downloadResult
	var mu sync.Mutex // Protected by mu: res.
	var fatal error
	fail := func(err error) {
		mu.Lock()
		if fatal == nil {
			fatal = err
		}
		mu.Unlock()
		cancel()
	}

	ch := make(chan job)
	var wg sync.WaitGroup
	for i := 0; i < s.opts.Workers; i++ {
		wg.Add(1)
		go func() { // Lifecycle: exits when ch closes or ctx is canceled; joined by wg.Wait.
			defer wg.Done()
			for j := range ch {
				s.worker(ctx, j, node, &res, &mu, fail)
			}
		}()
	}
	s.feed(ctx, ch, jobs)
	close(ch) // Closed by producer (downloadFolder).
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return res, fatal
}

// feed sends jobs until the list, limit or context ends.
func (s *Service) feed(ctx context.Context, ch chan<- job, jobs []job) {
	for _, j := range jobs {
		if s.limitReached() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case ch <- j:
		}
	}
}

// worker downloads one message and updates counters and the journal.
func (s *Service) worker(ctx context.Context, j job, node *layout.Node, res *downloadResult, mu *sync.Mutex, fail func(error)) {
	if ctx.Err() != nil || s.limitReached() {
		return
	}
	rel, size, err := s.downloadJob(ctx, j)
	if err != nil {
		if errors.Is(err, mailapi.ErrAuth) {
			fail(err)
			return
		}
		s.recordFailure(ctx, j, rel, err)
		mu.Lock()
		res.failed++
		mu.Unlock()
		s.failed.Add(1)
		s.logf("FAILED %s: %v", j.messageID, err)
	} else {
		s.recordSuccess(ctx, j, rel, size)
		mu.Lock()
		res.downloaded++
		res.bytes += size
		mu.Unlock()
		s.downloaded.Add(1)
		s.bytes.Add(size)
		s.totalDownloaded.Add(1)
	}
	if err := sleepCtx(ctx, s.opts.Delay); err != nil {
		return
	}
}

// downloadJob fetches and atomically stores one .eml file.
func (s *Service) downloadJob(ctx context.Context, j job) (string, int64, error) {
	name := layout.SafeFilename(j.dateUnix, j.subject, j.messageID)
	dest := filepath.Join(s.opts.AccountDir, filepath.FromSlash(j.folderPath), name)
	rel, err := filepath.Rel(s.opts.AccountDir, dest)
	if err != nil {
		return "", 0, fmt.Errorf("REQ-EXPORT-005: relative path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	data, err := s.fetch(ctx, j)
	if err != nil {
		return rel, 0, err
	}
	if err := writeFileAtomic(dest, data); err != nil {
		return rel, 0, fmt.Errorf("REQ-EXPORT-002: write %s: %w", rel, err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return rel, 0, fmt.Errorf("REQ-EXPORT-002: stat %s: %w", rel, err)
	}
	if st.Size() != int64(len(data)) {
		return rel, 0, fmt.Errorf("REQ-EXPORT-002: %s size %d, want %d", rel, st.Size(), len(data))
	}
	return rel, int64(len(data)), nil
}

// fetch downloads raw bytes with three attempts and bounded backoff.
func (s *Service) fetch(ctx context.Context, j job) ([]byte, error) {
	backoff := firstBackoff
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		data, err := s.api.DownloadRaw(ctx, j.messageID, j.folderID)
		if err == nil && mailapi.LooksLikeEml(data) {
			return data, nil
		}
		if err == nil {
			err = fmt.Errorf("response is not a valid eml (%d bytes)", len(data))
		}
		lastErr = err
		if errors.Is(err, mailapi.ErrAuth) || errors.Is(err, context.Canceled) {
			return nil, err
		}
		pause := backoff
		if errors.Is(err, mailapi.ErrRateLimited) {
			pause = rateLimitPause
		}
		if serr := sleepCtx(ctx, pause); serr != nil {
			return nil, serr
		}
		backoff *= backoffFactor
	}
	return nil, lastErr
}

// recordSuccess appends a downloaded entry.
func (s *Service) recordSuccess(ctx context.Context, j job, rel string, size int64) {
	s.record(ctx, journal.Entry{
		Folder:    j.folderPath,
		FolderID:  j.folderID,
		MessageID: j.messageID,
		Date:      formatDate(j.dateUnix),
		Subject:   j.subject,
		Sender:    j.sender,
		Dest:      rel,
		Status:    journal.StatusDownloaded,
		Size:      size,
	})
}

// recordFailure appends a failed entry with the error text.
func (s *Service) recordFailure(ctx context.Context, j job, rel string, cause error) {
	s.record(ctx, journal.Entry{
		Folder:    j.folderPath,
		FolderID:  j.folderID,
		MessageID: j.messageID,
		Date:      formatDate(j.dateUnix),
		Subject:   j.subject,
		Sender:    j.sender,
		Dest:      rel,
		Status:    journal.StatusFailed,
		Error:     cause.Error(),
	})
}

// record writes one journal entry even if the run context was canceled.
func (s *Service) record(ctx context.Context, e journal.Entry) {
	jctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), journalWriteTimeout)
	defer cancel()
	if err := s.journal.Record(jctx, e); err != nil {
		s.logf("journal: %v", err)
	}
}

// formatDate renders a unix timestamp for the journal.
func formatDate(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}

// writeFileAtomic writes data to a temporary sibling and renames it.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".part-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()     // NOERR: the temp file is removed below.
		os.Remove(name) // NOERR: best-effort cleanup.
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()     // NOERR: the temp file is removed below.
		os.Remove(name) // NOERR: best-effort cleanup.
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name) // NOERR: best-effort cleanup.
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name) // NOERR: best-effort cleanup.
		return err
	}
	return nil
}
