// Command mexporter exports Mail.ru mailboxes to .eml files.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"mexporter/internal/config"
	"mexporter/internal/exporter"
	"mexporter/internal/journal"
	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
	"mexporter/internal/session"
	"mexporter/internal/tui"
)

type cliFlags struct {
	sessionPath  string
	configPath   string
	outDir       string
	folderIDs    string
	workers      int
	limit        int
	pageSize     int
	delay        time.Duration
	listOnly     bool
	dryRun       bool
	verbose      bool
	noTUI        bool
	initSession  bool
	loginURL     string
	baseURL      string
	verify       bool
	verifyHash   bool
	jsonOut      bool
	allowPartial bool
	force        bool
}

const (
	exitOK       = 0
	exitFail     = 1
	exitAuth     = 2
	exitCanceled = 130
)

func main() {
	os.Exit(run(os.Args[1:]))
}

// run wires the process and maps errors to exit codes.
func run(args []string) int {
	fl, err := parseFlags(args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return exitOK
		}
		logf("FATAL: %v", err)
		return exitFail
	}
	if fl.initSession {
		if err := bootstrapSession(fl); err != nil {
			return fail(err)
		}
	}
	if fl.verifyHash {
		fl.verify = true
	}
	if fl.verify {
		return runVerify(fl)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := session.Load(fl.sessionPath)
	if err != nil {
		return fail(err)
	}
	if warning := session.ExpiryWarning(sess, time.Now()); warning != "" {
		logf("session warning: %s", warning)
	}
	cli := mailapi.New(mailapi.Options{
		Email:        sess.Email,
		AccessToken:  sess.AccessToken,
		CookieHeader: sess.CookieHeader(time.Now()),
		BaseURL:      fl.baseURL,
	})
	nodes, err := loadNodes(ctx, cli)
	if err != nil {
		return fail(err)
	}
	if fl.listOnly {
		if fl.jsonOut {
			printTreeJSON(nodes)
		} else {
			printTree(nodes)
		}
		return exitOK
	}

	cfg, err := config.Load(fl.configPath)
	if err != nil {
		return fail(err)
	}
	applyFlags(&cfg, fl)
	ids, err := chooseFolders(nodes, cfg, fl, sess.Email)
	if err != nil {
		if errors.Is(err, tui.ErrCanceled) {
			logf("selection canceled")
			return exitCanceled
		}
		return fail(err)
	}
	return execute(ctx, cli, nodes, ids, cfg, sess.Email, fl)
}

// parseFlags defines the command line surface.
func parseFlags(args []string) (cliFlags, error) {
	var fl cliFlags
	fs := flag.NewFlagSet("mexporter", flag.ContinueOnError)
	fs.StringVar(&fl.sessionPath, "session", "session/session.json", "session file with cookies and access_token")
	fs.StringVar(&fl.configPath, "config", "mexporter.json", "config file with the folder selection")
	fs.StringVar(&fl.outDir, "out", "", "output root (default: config or mail-export)")
	fs.StringVar(&fl.folderIDs, "folders", "", "comma-separated folder ids; skips the TUI")
	fs.IntVar(&fl.workers, "workers", 0, "parallel downloads (default: config)")
	fs.IntVar(&fl.limit, "limit", 0, "max messages in this run (0 = no limit)")
	fs.IntVar(&fl.pageSize, "page-size", 50, "threads per API page")
	fs.DurationVar(&fl.delay, "delay", 0, "pause per worker (default: config)")
	fs.BoolVar(&fl.listOnly, "list", false, "print the folder tree and exit")
	fs.BoolVar(&fl.dryRun, "dry-run", false, "enumerate messages without downloading")
	fs.BoolVar(&fl.verbose, "v", false, "log every downloaded message")
	fs.BoolVar(&fl.noTUI, "no-tui", false, "use config folders without the selector")
	fs.BoolVar(&fl.initSession, "init-session", false, "create or replace the session file via guided manual login/import")
	fs.StringVar(&fl.loginURL, "login-url", "https://e.mail.ru/", "URL to open in browser during -init-session")
	fs.StringVar(&fl.baseURL, "base-url", "", "mail API base URL (default: https://e.mail.ru/api/v1/)")
	fs.BoolVar(&fl.verify, "verify", false, "check the journal against existing files and exit")
	fs.BoolVar(&fl.verifyHash, "verify-hash", false, "verify file SHA-256 too (implies -verify)")
	fs.BoolVar(&fl.jsonOut, "json", false, "write machine-readable JSON to stdout")
	fs.BoolVar(&fl.allowPartial, "allow-partial", false, "exit 0 even when some folders or threads failed")
	fs.BoolVar(&fl.force, "force", false, "replace an existing session or stale lock")
	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err
	}
	return fl, nil
}

// loadNodes fetches the folder tree and flattens it in display order.
func loadNodes(ctx context.Context, cli *mailapi.Client) ([]*layout.Node, error) {
	folders, err := cli.Folders(ctx)
	if err != nil {
		return nil, fmt.Errorf("load folders: %w", err)
	}
	roots, _ := layout.BuildTree(folders)
	return layout.Flatten(roots), nil
}

// applyFlags lets explicit flags override persisted defaults.
func applyFlags(cfg *config.Config, fl cliFlags) {
	if fl.outDir != "" {
		cfg.OutDir = fl.outDir
	}
	if fl.workers > 0 {
		cfg.Workers = fl.workers
	}
	if fl.delay > 0 {
		cfg.DelayMS = int(fl.delay / time.Millisecond)
	}
	cfg.Normalize()
}

// chooseFolders returns a selection and persists it before the export starts.
func chooseFolders(nodes []*layout.Node, cfg config.Config, fl cliFlags, email string) ([]string, error) {
	var ids []string
	switch {
	case fl.folderIDs != "":
		ids = splitIDs(fl.folderIDs)
		valid, unknown := validateFolderIDs(nodes, ids)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("unknown folder id(s): %s", strings.Join(unknown, ", "))
		}
		ids = valid
	case fl.noTUI:
		valid, unknown := validateFolderIDs(nodes, cfg.Folders)
		for _, id := range unknown {
			logf("ignoring unknown configured folder id %q", id)
		}
		if len(valid) == 0 {
			return nil, errors.New("config has no known folder ids; run the selector or pass -folders")
		}
		return valid, nil
	default:
		selected, err := tui.Run(nodes, cfg.Folders, email)
		if err != nil {
			return nil, err
		}
		ids = selected
		valid, unknown := validateFolderIDs(nodes, ids)
		if len(unknown) > 0 {
			return nil, fmt.Errorf("selector returned unknown folder id(s): %s", strings.Join(unknown, ", "))
		}
		ids = valid
	}
	if len(ids) == 0 {
		return nil, errors.New("no folders selected")
	}
	cfg.Folders = ids
	cfg.Email = email
	if err := config.Save(fl.configPath, cfg, time.Now()); err != nil {
		return nil, fmt.Errorf("save selection: %w", err)
	}
	return ids, nil
}

// validateFolderIDs keeps known ids in input order and reports unknown ones.
func validateFolderIDs(nodes []*layout.Node, ids []string) (valid, unknown []string) {
	known := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		known[n.ID.String()] = true
	}
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if known[id] {
			valid = append(valid, id)
		} else {
			unknown = append(unknown, id)
		}
	}
	return valid, unknown
}

// splitIDs parses a comma-separated folder list.
func splitIDs(raw string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// execute opens the journal and runs the exporter.
func execute(ctx context.Context, cli *mailapi.Client, nodes []*layout.Node, ids []string, cfg config.Config, email string, fl cliFlags) int {
	if len(ids) == 0 {
		logf("no folders selected: pass -folders or run the selector")
		return exitFail
	}
	accountDir := filepath.Join(cfg.OutDir, layout.SanitizeName(email, 80))
	if err := os.MkdirAll(accountDir, 0o755); err != nil {
		return fail(fmt.Errorf("create output dir: %w", err))
	}
	release, err := acquireLock(accountDir, fl.force)
	if err != nil {
		return fail(err)
	}
	defer release()
	if err := exporter.WriteManifest(accountDir, nodes); err != nil {
		return fail(err)
	}
	j, err := journal.Open(accountDir, email)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if cerr := j.Close(); cerr != nil {
			logf("journal close: %v", cerr)
		}
	}()

	svc := exporter.New(cli, j, exporter.Options{
		AccountDir: accountDir,
		Folders:    ids,
		Workers:    cfg.Workers,
		Delay:      time.Duration(cfg.DelayMS) * time.Millisecond,
		Limit:      fl.limit,
		PageSize:   fl.pageSize,
		DryRun:     fl.dryRun,
		Verbose:    fl.verbose,
		Progress:   os.Stderr,
		Logf:       logf,
	})
	summary, err := svc.Run(ctx, nodes)
	if err != nil {
		logf("export stopped: %v", err)
		return exitFor(err)
	}
	if fl.jsonOut {
		printSummaryJSON(summary, accountDir)
	} else {
		printSummary(summary, accountDir)
	}
	if summary.Partial && !fl.allowPartial {
		logf("export is partial: %d failed folder(s), %d failed thread(s), %d partial thread(s)",
			summary.FailedFolders, summary.ThreadsFailed, summary.ThreadsPartial)
		logf("fix the cause and rerun, or pass -allow-partial to accept a partial result")
		return exitFail
	}
	return exitOK
}

// fail logs an error and maps it to an exit code.
func fail(err error) int {
	logf("FATAL: %v", err)
	return exitFor(err)
}

// exitFor maps an error to the documented process status.
func exitFor(err error) int {
	switch {
	case errors.Is(err, mailapi.ErrAuth):
		return exitAuth
	case errors.Is(err, context.Canceled):
		return exitCanceled
	default:
		return exitFail
	}
}

// printSummary reports totals and per-folder counters.
func printSummary(s exporter.Summary, accountDir string) {
	logf("done: found=%d downloaded=%d failed=%d skipped=%d", s.Found, s.Downloaded, s.Failed, s.Skipped)
	logf("threads: failed=%d partial=%d | failed folders=%d | partial=%t",
		s.ThreadsFailed, s.ThreadsPartial, s.FailedFolders, s.Partial)
	logf("total size: %s", humanBytes(s.Bytes))
	for _, f := range s.PerFolder {
		if f.Found == 0 && f.Downloaded == 0 && f.Failed == 0 &&
			f.ThreadsFailed == 0 && f.ThreadsPartial == 0 && !f.Partial {
			continue
		}
		logf("%s: found=%d ok=%d fail=%d skip=%d threads_fail=%d threads_partial=%d partial=%t",
			f.Path, f.Found, f.Downloaded, f.Failed, f.Skipped,
			f.ThreadsFailed, f.ThreadsPartial, f.Partial)
	}
	logf("journal: %s", filepath.Join(accountDir, "export-log.jsonl"))
}

// printSummaryJSON writes the run summary to stdout for scripts.
func printSummaryJSON(s exporter.Summary, accountDir string) {
	out := struct {
		AccountDir string `json:"account_dir"`
		exporter.Summary
	}{AccountDir: accountDir, Summary: s}
	raw, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		logf("encode summary: %v", err)
		return
	}
	fmt.Println(string(raw))
}

// printTreeJSON writes the folder tree to stdout for scripts.
func printTreeJSON(nodes []*layout.Node) {
	type row struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Parent   string `json:"parent"`
		Path     string `json:"path"`
		Messages int    `json:"messages"`
	}
	rows := make([]row, 0, len(nodes))
	for _, n := range nodes {
		rows = append(rows, row{
			ID:       n.ID.String(),
			Name:     n.Name,
			Parent:   n.Parent.String(),
			Path:     n.Path,
			Messages: n.MessagesTotal,
		})
	}
	raw, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		logf("encode tree: %v", err)
		return
	}
	fmt.Println(string(raw))
}

// printTree writes the folder table to stdout.
func printTree(nodes []*layout.Node) {
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tMESSAGES\tPATH")
	for _, n := range nodes {
		fmt.Fprintf(w, "%s\t%d\t%s\n", n.ID, n.MessagesTotal, n.Path)
	}
	if err := w.Flush(); err != nil {
		logf("print tree: %v", err)
	}
}

// humanBytes formats a byte count.
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

// runVerify checks the journal against files without contacting the API.
func runVerify(fl cliFlags) int {
	cfg, err := config.Load(fl.configPath)
	if err != nil {
		return fail(err)
	}
	applyFlags(&cfg, fl)
	email := cfg.Email
	if email == "" {
		sess, loadErr := session.Load(fl.sessionPath)
		if loadErr != nil {
			return fail(errors.New("verify needs an account: set email in config or provide a session file"))
		}
		email = sess.Email
	}
	accountDir := filepath.Join(cfg.OutDir, layout.SanitizeName(email, 80))
	if _, err := os.Stat(accountDir); err != nil {
		return fail(fmt.Errorf("verify: account directory: %w", err))
	}
	if !journalExists(accountDir) {
		return fail(fmt.Errorf("verify: no journal in %s", accountDir))
	}
	j, err := journal.OpenReadOnly(accountDir, email)
	if err != nil {
		return fail(err)
	}
	rep, err := j.Verify(accountDir, fl.verifyHash)
	if err != nil {
		return fail(err)
	}
	if fl.jsonOut {
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Println(string(raw))
	} else {
		logf("verify: total=%d ok=%d missing=%d size=%d hash=%d no-hash=%d unsafe=%d unreadable=%d",
			rep.Total, rep.OK, rep.Missing, rep.SizeMismatch, rep.HashMismatch, rep.NoHash, rep.Unsafe, rep.Unreadable)
		for _, issue := range rep.Issues {
			logf("  %s %s: %s %s", issue.MessageID, issue.Kind, issue.Dest, issue.Detail)
		}
		if rep.Truncated {
			logf("  issue list truncated; fix the first problems and rerun")
		}
	}
	if rep.Missing+rep.SizeMismatch+rep.HashMismatch+rep.Unsafe+rep.Unreadable > 0 {
		return exitFail
	}
	return exitOK
}

func journalExists(accountDir string) bool {
	for _, name := range []string{"export-log.jsonl", "export-log.json"} {
		if _, err := os.Stat(filepath.Join(accountDir, name)); err == nil {
			return true
		}
	}
	return false
}

// bootstrapSession captures the session from a debuggable browser and saves it.
// When the browser cannot be driven automatically it falls back to the manual
// JSON import, so -init-session keeps working without Chrome.
func bootstrapSession(fl cliFlags) error {
	if !fl.force {
		if _, err := os.Stat(fl.sessionPath); err == nil {
			return fmt.Errorf("session file already exists: %s (pass -force to replace it)", fl.sessionPath)
		}
	}
	fmt.Fprintln(os.Stderr, "Session setup (mode 0)")
	fmt.Fprintf(os.Stderr, "Target session file: %s\n", fl.sessionPath)

	r := bufio.NewReader(os.Stdin)
	s, err := captureSession(fl, r, os.Stderr)
	if err != nil {
		if !errors.Is(err, errCaptureUnavailable) {
			return err
		}
		logf("automatic capture unavailable: %v", err)
		s, err = manualSession(fl, r)
		if err != nil {
			return err
		}
	}
	if err := session.Save(fl.sessionPath, s); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Session saved: %s (account: %s)\n", fl.sessionPath, s.Email)
	return nil
}

// manualSession opens the login page in the default browser and walks the user
// through importing a session JSON document from a file or pasted input.
func manualSession(fl cliFlags, r *bufio.Reader) (session.Session, error) {
	if fl.loginURL != "" {
		fmt.Fprintf(os.Stderr, "Opening browser: %s\n", fl.loginURL)
		if err := openBrowser(fl.loginURL); err != nil {
			logf("could not open browser automatically: %v", err)
			fmt.Fprintln(os.Stderr, "Open the login URL manually in your browser.")
		}
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "1. Sign in to the mailbox in the browser.")
	fmt.Fprintln(os.Stderr, "2. Export session data as JSON with fields: email, access_token, cookies (and csrf_token if available).")
	fmt.Fprintln(os.Stderr, "3. Save that JSON to a file, or paste it below.")
	fmt.Fprintln(os.Stderr)
	return promptSession(r, os.Stderr)
}

// promptSession reads session JSON from a file path or pasted stdin.
func promptSession(in io.Reader, out io.Writer) (session.Session, error) {
	r := bufio.NewReader(in)
	fmt.Fprint(out, "Path to session JSON file (leave empty to paste JSON): ")
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return session.Session{}, fmt.Errorf("read session input: %w", err)
	}
	path := strings.TrimSpace(line)
	var raw []byte
	if path != "" {
		raw, err = os.ReadFile(path)
		if err != nil {
			return session.Session{}, fmt.Errorf("read session file: %w", err)
		}
	} else {
		fmt.Fprintln(out, "Paste session JSON, then enter a single '.' on a new line:")
		var b strings.Builder
		for {
			l, e := r.ReadString('\n')
			if len(l) > 0 {
				trimmed := strings.TrimRight(l, "\r\n")
				if trimmed == "." {
					break
				}
				b.WriteString(l)
			}
			if e != nil {
				if errors.Is(e, io.EOF) {
					break
				}
				return session.Session{}, fmt.Errorf("read pasted session JSON: %w", e)
			}
		}
		raw = []byte(strings.TrimSpace(b.String()))
	}
	if len(raw) == 0 {
		return session.Session{}, errors.New("session JSON is empty")
	}
	var s session.Session
	if err := json.Unmarshal(raw, &s); err != nil {
		return session.Session{}, fmt.Errorf("parse session JSON: %w", err)
	}
	if err := session.Validate(s); err != nil {
		return session.Session{}, err
	}
	return s, nil
}

// openBrowser tries to open a URL in the default browser.
func openBrowser(url string) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("empty URL")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

// acquireLock prevents two exporters from writing the same account directory.
// A lock older than 24 hours is treated as stale; -force removes any lock.
func acquireLock(accountDir string, force bool) (func(), error) {
	path := filepath.Join(accountDir, ".mexporter.lock")
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create export lock: %w", err)
		}
		info, statErr := os.Stat(path)
		if statErr != nil {
			return nil, fmt.Errorf("inspect export lock: %w", statErr)
		}
		if force || time.Since(info.ModTime()) > 24*time.Hour {
			if force {
				logf("removing existing export lock %s", path)
			} else {
				logf("removing stale export lock %s", path)
			}
			if rmErr := os.Remove(path); rmErr != nil {
				return nil, fmt.Errorf("remove export lock: %w", rmErr)
			}
			continue
		}
		return nil, fmt.Errorf("another exporter is using %s; wait for it to finish or pass -force", accountDir)
	}
}

// logf writes a timestamped line to stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
