package exporter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

// worker downloads one message and updates counters and the journal.
func (s *Service) worker(ctx context.Context, j job, res *downloadResult, mu *sync.Mutex, fail func(error)) {
	if ctx.Err() != nil {
		return
	}
	rel, size, sum, err := s.downloadJob(ctx, j)
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
		s.recordSuccess(ctx, j, rel, size, sum)
		mu.Lock()
		res.downloaded++
		res.bytes += size
		mu.Unlock()
		s.downloaded.Add(1)
		s.bytes.Add(size)
		if s.opts.Verbose {
			s.logf("OK %s (%d bytes)", j.messageID, size)
		}
	}
	if err := sleepCtx(ctx, s.opts.Delay); err != nil {
		return
	}
}

// downloadJob fetches and atomically stores one .eml file.
func (s *Service) downloadJob(ctx context.Context, j job) (string, int64, string, error) {
	name := layout.SafeFilename(j.dateUnix, j.subject, j.messageID)
	dest := filepath.Join(s.opts.AccountDir, filepath.FromSlash(j.folderPath), name)
	rel, err := filepath.Rel(s.opts.AccountDir, dest)
	if err != nil {
		return "", 0, "", fmt.Errorf("REQ-EXPORT-005: relative path: %w", err)
	}
	rel = filepath.ToSlash(rel)
	data, err := s.fetch(ctx, j)
	if err != nil {
		return rel, 0, "", err
	}
	if err := writeFileAtomic(dest, data); err != nil {
		return rel, 0, "", fmt.Errorf("REQ-EXPORT-002: write %s: %w", rel, err)
	}
	st, err := os.Stat(dest)
	if err != nil {
		return rel, 0, "", fmt.Errorf("REQ-EXPORT-002: stat %s: %w", rel, err)
	}
	if st.Size() != int64(len(data)) {
		return rel, 0, "", fmt.Errorf("REQ-EXPORT-002: %s size %d, want %d", rel, st.Size(), len(data))
	}
	sum := sha256.Sum256(data)
	return rel, int64(len(data)), hex.EncodeToString(sum[:]), nil
}

// fetch downloads raw bytes and retries only when the server returned
// something that is not an .eml. Transport retries belong to mailapi.
func (s *Service) fetch(ctx context.Context, j job) ([]byte, error) {
	backoff := retryFirstBackoff
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		raw, err := s.api.DownloadRaw(ctx, j.messageID, j.folderID)
		if err != nil {
			return nil, err
		}
		if mailapi.LooksLikeEml(raw) {
			return raw, nil
		}
		last = fmt.Errorf("response is not a valid eml (%d bytes)", len(raw))
		if attempt == maxAttempts {
			break
		}
		if err := sleepCtx(ctx, backoff); err != nil {
			return nil, err
		}
		backoff *= backoffFactor
	}
	return nil, last
}

// recordSuccess appends a downloaded entry.
func (s *Service) recordSuccess(ctx context.Context, j job, rel string, size int64, sum string) {
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
		SHA256:    sum,
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
