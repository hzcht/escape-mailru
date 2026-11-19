// Command mexporter exports Mail.ru mailboxes to .eml files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
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
}

func main() {
	os.Exit(run())
}

// run wires the process and maps errors to exit codes.
func run() int {
	fl := parseFlags()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sess, err := session.Load(fl.sessionPath)
	if err != nil {
		logf("FATAL: %v", err)
		return 1
	}
	cli := mailapi.New(mailapi.Options{
		Email:        sess.Email,
		AccessToken:  sess.AccessToken,
		CookieHeader: sess.CookieHeader(time.Now()),
	})
	nodes, err := loadNodes(ctx, cli, fl.pageSize)
	if err != nil {
		logf("FATAL: %v", err)
		return 1
	}
	if fl.listOnly {
		printTree(nodes)
		return 0
	}

	cfg, err := config.Load(fl.configPath)
	if err != nil {
		logf("FATAL: %v", err)
		return 1
	}
	applyFlags(&cfg, fl)
	ids, err := chooseFolders(nodes, cfg, fl, sess.Email)
	if err != nil {
		if errors.Is(err, tui.ErrCanceled) {
			logf("selection canceled")
			return 0
		}
		logf("FATAL: %v", err)
		return 1
	}
	return execute(ctx, cli, nodes, ids, cfg, sess.Email, fl)
}

// parseFlags defines the command line surface.
func parseFlags() cliFlags {
	var fl cliFlags
	flag.StringVar(&fl.sessionPath, "session", "session/session.json", "session file with cookies and access_token")
	flag.StringVar(&fl.configPath, "config", "mexporter.json", "config file with the folder selection")
	flag.StringVar(&fl.outDir, "out", "", "output root (default: config or mail-export)")
	flag.StringVar(&fl.folderIDs, "folders", "", "comma-separated folder ids; skips the TUI")
	flag.IntVar(&fl.workers, "workers", 0, "parallel downloads (default: config)")
	flag.IntVar(&fl.limit, "limit", 0, "max messages in this run (0 = no limit)")
	flag.IntVar(&fl.pageSize, "page-size", 50, "threads per API page")
	flag.DurationVar(&fl.delay, "delay", 0, "pause per worker (default: config)")
	flag.BoolVar(&fl.listOnly, "list", false, "print the folder tree and exit")
	flag.BoolVar(&fl.dryRun, "dry-run", false, "enumerate messages without downloading")
	flag.BoolVar(&fl.verbose, "v", false, "verbose diagnostics")
	flag.BoolVar(&fl.noTUI, "no-tui", false, "use config folders without the selector")
	flag.Parse()
	return fl
}

// loadNodes fetches the folder tree and flattens it in display order.
func loadNodes(ctx context.Context, cli *mailapi.Client, pageSize int) ([]*layout.Node, error) {
	if pageSize <= 0 {
		pageSize = 50
	}
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
		return 1
	}
	accountDir := filepath.Join(cfg.OutDir, layout.SanitizeName(email, 80))
	if err := os.MkdirAll(accountDir, 0o755); err != nil {
		logf("FATAL: create output dir: %v", err)
		return 1
	}
	if err := exporter.WriteManifest(accountDir, nodes); err != nil {
		logf("FATAL: %v", err)
		return 1
	}
	j, err := journal.Open(accountDir, email)
	if err != nil {
		logf("FATAL: %v", err)
		return 1
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
		if errors.Is(err, mailapi.ErrAuth) {
			return 2
		}
		if errors.Is(err, context.Canceled) {
			return 130
		}
		return 1
	}
	printSummary(summary, accountDir)
	return 0
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

// logf writes a timestamped line to stderr.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().Format("15:04:05"), fmt.Sprintf(format, args...))
}
