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
	sessionPath string
	configPath  string
	outDir      string
	folderIDs   string
	workers     int
	limit       int
	pageSize    int
	delay       time.Duration
	listOnly    bool
	dryRun      bool
	verbose     bool
	noTUI       bool
	initSession bool
	loginURL    string
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := session.Load(fl.sessionPath)
	if err != nil {
		return fail(err)
	}
	cli := mailapi.New(mailapi.Options{
		Email:        sess.Email,
		AccessToken:  sess.AccessToken,
		CookieHeader: sess.CookieHeader(time.Now()),
	})
	nodes, err := loadNodes(ctx, cli)
	if err != nil {
		return fail(err)
	}
	if fl.listOnly {
		printTree(nodes)
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
			return exitOK
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
	case fl.noTUI:
		return cfg.Folders, nil
	default:
		selected, err := tui.Run(nodes, cfg.Folders, email)
		if err != nil {
			return nil, err
		}
		ids = selected
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
	printSummary(summary, accountDir)
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
	logf("total size: %s", humanBytes(s.Bytes))
	for _, f := range s.PerFolder {
		if f.Found == 0 && f.Downloaded == 0 && f.Failed == 0 {
			continue
		}
		logf("%s: found=%d ok=%d fail=%d skip=%d", f.Path, f.Found, f.Downloaded, f.Failed, f.Skipped)
	}
	logf("journal: %s", filepath.Join(accountDir, "export-log.json"))
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

// bootstrapSession guides manual login and saves a session file.
func bootstrapSession(fl cliFlags) error {
	fmt.Fprintln(os.Stderr, "Session setup (mode 0)")
	fmt.Fprintf(os.Stderr, "Target session file: %s\n", fl.sessionPath)
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

	s, err := promptSession(os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	if err := session.Save(fl.sessionPath, s); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "Session saved: %s (account: %s)\n", fl.sessionPath, s.Email)
	return nil
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

// logf writes a timestamped line to stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
