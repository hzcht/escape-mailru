package exporter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

type enumStats struct {
	ThreadsFailed  int
	ThreadsPartial int
	Truncated      bool
}

// enumerateFolder walks all pages and streams unique messages to emit.
func (s *Service) enumerateFolder(ctx context.Context, node *layout.Node, emit func(job) error) (enumStats, error) {
	var stats enumStats
	folderID := node.ID.String()
	seenThreads := make(map[string]struct{})
	stale := 0
	for offset := 0; offset <= maxEnumerationOffset; {
		body, err := s.api.ListPage(ctx, folderID, offset, s.opts.PageSize, mailapi.OrderAsc)
		if err != nil {
			return stats, fmt.Errorf("REQ-EXPORT-001: list %q offset %d: %w", node.Path, offset, err)
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
			msgs, failed, partial, err := s.resolveThread(ctx, t)
			if err != nil {
				return stats, err
			}
			if failed {
				stats.ThreadsFailed++
				s.logf("thread %s: expansion failed; it will be retried on the next run", t.ID)
				continue
			}
			if partial {
				stats.ThreadsPartial++
				s.logf("thread %s: expanded %d of %d messages; exporting what the API returned", t.ID, len(msgs), t.Length)
			}
			for _, m := range msgs {
				if m.ID == "" {
					continue
				}
				if _, ok := s.seen[m.ID]; ok {
					continue
				}
				s.seen[m.ID] = struct{}{}
				msgFolderID := m.Folder.String()
				if msgFolderID == "" {
					msgFolderID = folderID
				}
				err := emit(job{
					messageID:  m.ID,
					folderID:   msgFolderID,
					folderPath: node.Path,
					subject:    m.Subject,
					sender:     m.Correspondents.Sender(),
					dateUnix:   m.Date,
				})
				if errors.Is(err, errLimitReached) {
					return stats, nil
				}
				if err != nil {
					return stats, err
				}
			}
		}
		stale = nextStale(stale, newThreads)
		if stale >= maxStalePages {
			stats.Truncated = true
			s.logf("folder %q: pagination made no progress at offset %d", node.Path, offset)
			break
		}
		// A pinned-only page does not advance the regular offset.
		if len(body.Threads) == 0 {
			break
		}
		offset += len(body.Threads)
		if offset > maxEnumerationOffset {
			stats.Truncated = true
			s.logf("folder %q: reached the enumeration offset limit at %d", node.Path, offset)
			break
		}
		if len(body.Threads) < s.opts.PageSize && len(body.Pinned.Messages) == 0 {
			break
		}
		if err := sleepCtx(ctx, pagePause); err != nil {
			return stats, err
		}
	}
	return stats, nil
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

// resolveThread expands multi-message threads. A failed thread is reported
// instead of silently falling back to its newest message.
func (s *Service) resolveThread(ctx context.Context, t mailapi.Thread) ([]mailapi.Message, bool, bool, error) {
	last := t.Last
	if t.Expand != nil && *t.Expand != "" {
		last = *t.Expand
	}
	if t.Length <= 1 {
		msgs := fallbackMessage(t, last)
		if len(msgs) == 0 {
			return nil, true, false, nil
		}
		return msgs, false, false, nil
	}
	msgs, err := s.api.ThreadMessages(ctx, t.ID)
	if err != nil {
		if errors.Is(err, mailapi.ErrAuth) || errors.Is(err, context.Canceled) {
			return nil, false, false, err
		}
		s.logf("thread %s: expand failed: %v", t.ID, err)
		return nil, true, false, nil
	}
	if len(msgs) == 0 {
		s.logf("thread %s: expand returned no messages", t.ID)
		return nil, true, false, nil
	}
	partial := t.Length > 0 && len(msgs) < t.Length
	return msgs, false, partial, nil
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
