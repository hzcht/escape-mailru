package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Job struct {
	MessageID  string
	FolderID   string
	FolderPath string
	Subject    string
	Sender     string
	DateUnix   int64
}

type counters struct {
	found     atomic.Int64
	ok        atomic.Int64
	failed    atomic.Int64
	skipped   atomic.Int64
	bytes     atomic.Int64
	processed atomic.Int64
	current   atomic.Value // string
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}

func main() {
	var (
		sessionPath = flag.String("session", "session/session.json", "path to session.json with cookies and access_token")
		outDir      = flag.String("out", "mail-export", "output root directory")
		workers     = flag.Int("workers", 4, "parallel downloads")
		delay       = flag.Duration("delay", 150*time.Millisecond, "pause per worker between downloads")
		limit       = flag.Int("limit", 0, "max messages to download in this run (0 = no limit)")
		folderIDs   = flag.String("folders", "", "comma-separated folder ids to export (default: all)")
		listOnly    = flag.Bool("list", false, "print the folder tree and exit")
		dryRun      = flag.Bool("dry-run", false, "enumerate messages but do not download")
		pageLimit   = flag.Int("page-size", 50, "threads per API page")
		verbose     = flag.Bool("v", false, "verbose listing")
	)
	flag.Parse()

	sess, err := LoadSession(*sessionPath)
	if err != nil {
		logf("FATAL: %v", err)
		os.Exit(1)
	}
	client := NewClient(sess)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		logf("interrupt received, finishing current downloads...")
		cancel()
	}()

	folders, err := client.Folders(ctx)
	if err != nil {
		logf("FATAL: load folder tree: %v", err)
		os.Exit(1)
	}
	roots, _ := BuildTree(folders)
	ordered := Flatten(roots)

	if *listOnly {
		fmt.Printf("%-8s %10s  %s\n", "ID", "MESSAGES", "PATH")
		for _, n := range ordered {
			fmt.Printf("%-8s %10d  %s\n", n.ID, n.MessagesTotal, n.Path)
		}
		return
	}

	filter := map[string]bool{}
	if *folderIDs != "" {
		for _, part := range strings.Split(*folderIDs, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				filter[part] = true
			}
		}
	}

	accountDir := filepath.Join(*outDir, SanitizeName(sess.Email, 80))
	if err := os.MkdirAll(accountDir, 0o755); err != nil {
		logf("FATAL: create output dir: %v", err)
		os.Exit(1)
	}
	writeFolderManifest(accountDir, ordered)

	journal, err := OpenJournal(accountDir, sess.Email)
	if err != nil {
		logf("FATAL: open journal: %v", err)
		os.Exit(1)
	}
	defer journal.Close()

	logf("account: %s", sess.Email)
	logf("folders: %d (%d with messages)", len(ordered), countNonEmpty(ordered))
	logf("journal: %s", journal)

	cnt := &counters{}
	stopProgress := make(chan struct{})
	go progressLoop(cnt, stopProgress)

	var fatalErr atomic.Value
	var totalDownloaded atomic.Int64
	seen := map[string]bool{}

	for _, node := range ordered {
		if ctx.Err() != nil {
			break
		}
		folderID := string(node.ID)
		if len(filter) > 0 && !filter[folderID] {
			continue
		}
		cnt.current.Store(node.Path)

		destDir := filepath.Join(accountDir, filepath.FromSlash(node.Path))
		if err := os.MkdirAll(destDir, 0o755); err != nil {
			logf("folder %s: cannot create dir: %v", node.Path, err)
			continue
		}
		if node.MessagesTotal == 0 {
			continue
		}

		logf("folder %q: enumerating %d messages", node.Path, node.MessagesTotal)
		jobs, err := enumerateFolder(ctx, client, node, *pageLimit, seen, *verbose)
		if err != nil {
			if errors.Is(err, ErrAuth) {
				fatalErr.Store(err)
				cancel()
				break
			}
			logf("folder %q: enumeration error: %v", node.Path, err)
			continue
		}
		cnt.found.Add(int64(len(jobs)))

		// Skip already downloaded messages.
		pending := make([]Job, 0, len(jobs))
		for _, job := range jobs {
			if e, ok := journal.Status(job.MessageID); ok && e.Status == StatusDownloaded {
				full := filepath.Join(accountDir, filepath.FromSlash(e.Dest))
				if st, err := os.Stat(full); err == nil && st.Size() > 0 {
					cnt.skipped.Add(1)
					continue
				}
			}
			pending = append(pending, job)
		}
		logf("folder %q: %d to download, %d already present", node.Path, len(pending), len(jobs)-len(pending))

		if *dryRun {
			continue
		}

		downloadFolder(ctx, client, journal, pending, destDir, accountDir, *workers, *delay, cnt, &fatalErr, &totalDownloaded, *limit)
		if fatalErr.Load() != nil {
			break
		}
		if *limit > 0 && totalDownloaded.Load() >= int64(*limit) {
			logf("limit of %d messages reached", *limit)
			break
		}
		journal.Save()
	}

	close(stopProgress)
	if err := journal.Save(); err != nil {
		logf("journal save: %v", err)
	}
	printSummary(journal, cnt)

	if v := fatalErr.Load(); v != nil {
		if errors.Is(v.(error), ErrAuth) {
			logf("session expired: re-extract session.json and run again")
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func countNonEmpty(nodes []*FolderNode) int {
	n := 0
	for _, x := range nodes {
		if x.MessagesTotal > 0 {
			n++
		}
	}
	return n
}

func writeFolderManifest(accountDir string, nodes []*FolderNode) {
	type manifest struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Parent   string `json:"parent"`
		Path     string `json:"path"`
		Messages int    `json:"messages"`
	}
	out := make([]manifest, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, manifest{ID: string(n.ID), Name: n.Name, Parent: string(n.Parent), Path: n.Path, Messages: n.MessagesTotal})
	}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(accountDir, "folders.json"), raw, 0o644)
}

// enumerateFolder walks all thread pages of a folder and returns unique messages.
func enumerateFolder(ctx context.Context, c *Client, node *FolderNode, pageSize int, seen map[string]bool, verbose bool) ([]Job, error) {
	folderID := string(node.ID)
	var jobs []Job
	seenThreads := map[string]bool{}
	offset := 0
	stalePages := 0
	for page := 0; ; page++ {
		if ctx.Err() != nil {
			return jobs, ctx.Err()
		}
		body, err := c.ListPage(ctx, folderID, offset, pageSize)
		if err != nil {
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.Status == 403 {
				return jobs, ErrAuth
			}
			return jobs, err
		}
		threads := append(append([]Thread{}, body.Threads...), body.Pinned.Messages...)
		if len(threads) == 0 {
			break
		}
		newThreads := 0
		for _, t := range threads {
			if t.ID == "" {
				continue // collector pseudo-row such as the whole "Р Р°СЃСЃС‹Р»РєРё" folder
			}
			if seenThreads[t.ID] {
				continue
			}
			seenThreads[t.ID] = true
			newThreads++

			msgs := resolveThread(ctx, c, t)
			for _, m := range msgs {
				if m.ID == "" || seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				folderOfMsg := string(m.Folder)
				if folderOfMsg == "" {
					folderOfMsg = folderID
				}
				jobs = append(jobs, Job{
					MessageID:  m.ID,
					FolderID:   folderOfMsg,
					FolderPath: node.Path,
					Subject:    m.Subject,
					Sender:     m.Correspondents.Sender(),
					DateUnix:   m.Date,
				})
			}
		}
		if verbose {
			logf("  page %d: %d threads, %d new, total messages %d", page, len(threads), newThreads, len(jobs))
		}
		if newThreads == 0 {
			stalePages++
			if stalePages >= 2 {
				break
			}
		} else {
			stalePages = 0
		}
		offset += len(body.Threads)
		if len(body.Threads) < pageSize {
			// Last page may still contain pinned rows; one extra page resolves it.
			if len(body.Pinned.Messages) == 0 {
				break
			}
		}
		if offset > 2_000_000 {
			break
		}
		time.Sleep(120 * time.Millisecond)
	}
	return jobs, nil
}

func resolveThread(ctx context.Context, c *Client, t Thread) []Message {
	last := t.Last
	if t.Expand != nil && *t.Expand != "" {
		last = *t.Expand
	}
	if t.Length <= 1 {
		if last == "" {
			return nil
		}
		return []Message{{
			ID:             last,
			Subject:        t.Subject,
			Date:           t.Date,
			Folder:         t.Folder,
			Correspondents: t.Correspondents,
		}}
	}
	msgs, err := c.ThreadMessages(ctx, t.ID)
	if err != nil || len(msgs) == 0 {
		if err != nil {
			logf("thread %s: expand failed (%v), using newest message only", t.ID, err)
		}
		if last == "" {
			return nil
		}
		return []Message{{
			ID:             last,
			Subject:        t.Subject,
			Date:           t.Date,
			Folder:         t.Folder,
			Correspondents: t.Correspondents,
		}}
	}
	return msgs
}

func downloadFolder(ctx context.Context, c *Client, j *Journal, jobs []Job, destDir, accountDir string, workers int, delay time.Duration, cnt *counters, fatalErr *atomic.Value, totalDownloaded *atomic.Int64, limit int) {
	ch := make(chan Job)
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range ch {
				if ctx.Err() != nil {
					return
				}
				if limit > 0 && totalDownloaded.Load() >= int64(limit) {
					continue
				}
				entry, err := downloadOne(ctx, c, job, destDir, accountDir)
				if err != nil {
					if errors.Is(err, ErrAuth) {
						fatalErr.Store(err)
						return
					}
					cnt.failed.Add(1)
					_ = j.Record(Entry{
						Folder:    job.FolderPath,
						FolderID:  job.FolderID,
						MessageID: job.MessageID,
						Date:      formatUnix(job.DateUnix),
						Subject:   job.Subject,
						Sender:    job.Sender,
						Dest:      entry.Dest,
						Status:    StatusFailed,
						Error:     err.Error(),
					})
					logf("FAILED %s: %v", job.MessageID, err)
				} else {
					cnt.ok.Add(1)
					cnt.bytes.Add(entry.Size)
					cnt.processed.Add(1)
					totalDownloaded.Add(1)
					_ = j.Record(entry)
				}
				time.Sleep(delay)
			}
		}()
	}

	for _, job := range jobs {
		if ctx.Err() != nil {
			break
		}
		if limit > 0 && totalDownloaded.Load() >= int64(limit) {
			break
		}
		ch <- job
	}
	close(ch)
	wg.Wait()
}

func downloadOne(ctx context.Context, c *Client, job Job, destDir, accountDir string) (Entry, error) {
	filename := SafeFilename(job.DateUnix, job.Subject, job.MessageID)
	dest := filepath.Join(destDir, filename)
	rel, _ := filepath.Rel(accountDir, dest)
	rel = filepath.ToSlash(rel)

	var lastErr error
	backoff := time.Second
	for attempt := 1; attempt <= 3; attempt++ {
		data, err := c.DownloadRaw(ctx, job.MessageID, job.FolderID)
		if err == nil && LooksLikeEml(data) {
			if werr := writeAtomic(dest, data); werr != nil {
				return Entry{Dest: rel}, werr
			}
			return Entry{
				Folder:    job.FolderPath,
				FolderID:  job.FolderID,
				MessageID: job.MessageID,
				Date:      formatUnix(job.DateUnix),
				Subject:   job.Subject,
				Sender:    job.Sender,
				Dest:      rel,
				Status:    StatusDownloaded,
				Size:      int64(len(data)),
			}, nil
		}
		if err == nil {
			lastErr = fmt.Errorf("response is not a valid eml (%d bytes)", len(data))
		} else {
			lastErr = err
		}
		if errors.Is(lastErr, ErrAuth) {
			return Entry{Dest: rel}, lastErr
		}
		if errors.Is(lastErr, ErrRateLimited) {
			time.Sleep(30 * time.Second)
		} else {
			time.Sleep(backoff)
		}
		backoff *= 3
	}
	return Entry{Dest: rel}, lastErr
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func formatUnix(ts int64) string {
	if ts <= 0 {
		return ""
	}
	return time.Unix(ts, 0).Format(time.RFC3339)
}

func progressLoop(cnt *counters, stop <-chan struct{}) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var lastBytes int64
	var lastTime = time.Now()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			cur, _ := cnt.current.Load().(string)
			b := cnt.bytes.Load()
			dt := time.Since(lastTime).Seconds()
			speed := float64(b-lastBytes) / 1024 / dt
			lastBytes = b
			lastTime = time.Now()
			fmt.Fprintf(os.Stderr, "\r[%s] %s | ok=%d fail=%d skip=%d | %.1f MB | %.0f KB/s   ",
				time.Now().Format("15:04:05"), truncate(cur, 40), cnt.ok.Load(), cnt.failed.Load(), cnt.skipped.Load(),
				float64(b)/1024/1024, speed)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "вЂ¦"
}

func printSummary(j *Journal, cnt *counters) {
	fmt.Fprintln(os.Stderr)
	s := j.Stats()
	logf("done: found=%d downloaded=%d failed=%d skipped_total=%d", cnt.found.Load(), s.Downloaded, s.Failed, cnt.skipped.Load())
	logf("total size: %s", humanBytes(s.Bytes))
	logf("journal: %s", filepath.Join("mail-export", "export-log.json"))
}

func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(b, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

var _ = sort.Strings
