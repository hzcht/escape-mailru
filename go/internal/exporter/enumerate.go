package exporter

import (
	"context"
	"fmt"
	"time"

	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

// enumerateFolder walks all pages and returns unique messages.
func (s *Service) enumerateFolder(ctx context.Context, node *layout.Node) ([]job, error) {
	folderID := node.ID.String()
	var jobs []job
	seenThreads := make(map[string]struct{})
	stale := 0
	for offset := 0; offset <= maxEnumerationOffset; {
		body, err := s.api.ListPage(ctx, folderID, offset, s.opts.PageSize, mailapi.OrderAsc)
		if err != nil {
			return jobs, fmt.Errorf("REQ-EXPORT-001: list %q offset %d: %w", node.Path, offset, err)
		}
		threads := joinThreads(body)
		if len(threads) == 0 {
			break
		}
		newThreads := 0
		for _, t := range threads {
			if t.ID == "" {
				continue // Collector pseudo-row for a whole folder.
			}
			if _, ok := seenThreads[t.ID]; ok {
				continue
			}
			seenThreads[t.ID] = struct{}{}
			newThreads++
			jobs = s.appendThread(ctx, jobs, node, t)
		}
		stale = nextStale(stale, newThreads)
		if stale >= maxStalePages {
			break
		}
		offset += len(body.Threads)
		if len(body.Threads) < s.opts.PageSize && len(body.Pinned.Messages) == 0 {
			break
		}
		if err := sleepCtx(ctx, pagePause); err != nil {
			return jobs, err
		}
	}
	return jobs, nil
}

// joinThreads merges regular and pinned rows without mutating the response.
func joinThreads(body *mailapi.ListBody) []mailapi.Thread {
	threads := make([]mailapi.Thread, 0, len(body.Threads)+len(body.Pinned.Messages))
	threads = append(threads, body.Threads...)
	threads = append(threads, body.Pinned.Messages...)
	return threads
}

// nextStale increments the stale counter when a page adds no threads.
func nextStale(stale, newThreads int) int {
	if newThreads == 0 {
		return stale + 1
	}
	return 0
}

// appendThread resolves a thread and appends its new messages as jobs.
func (s *Service) appendThread(ctx context.Context, jobs []job, node *layout.Node, t mailapi.Thread) []job {
	for _, m := range s.resolveThread(ctx, t) {
		if m.ID == "" {
			continue
		}
		if _, ok := s.seen[m.ID]; ok {
			continue
		}
		s.seen[m.ID] = struct{}{}
		folderID := m.Folder.String()
		if folderID == "" {
			folderID = node.ID.String()
		}
		jobs = append(jobs, job{
			messageID:  m.ID,
			folderID:   folderID,
			folderPath: node.Path,
			subject:    m.Subject,
			sender:     m.Correspondents.Sender(),
			dateUnix:   m.Date,
		})
	}
	return jobs
}

// resolveThread expands multi-message threads, falling back to the newest message.
func (s *Service) resolveThread(ctx context.Context, t mailapi.Thread) []mailapi.Message {
	last := t.Last
	if t.Expand != nil && *t.Expand != "" {
		last = *t.Expand
	}
	if t.Length <= 1 {
		return fallbackMessage(t, last)
	}
	msgs, err := s.api.ThreadMessages(ctx, t.ID)
	if err != nil {
		s.logf("thread %s: expand failed (%v), using newest message only", t.ID, err)
		return fallbackMessage(t, last)
	}
	if len(msgs) == 0 {
		return fallbackMessage(t, last)
	}
	return msgs
}

// fallbackMessage builds a synthetic message from the thread row.
func fallbackMessage(t mailapi.Thread, id string) []mailapi.Message {
	if id == "" {
		return nil
	}
	return []mailapi.Message{{
		ID:             id,
		Subject:        t.Subject,
		Date:           t.Date,
		Folder:         t.Folder,
		Correspondents: t.Correspondents,
	}}
}

// sleepCtx waits for d or until the context ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
