package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mexporter/internal/config"
	"mexporter/internal/exporter"
	"mexporter/internal/journal"
	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
	"mexporter/internal/session"
)

func TestParseFlags(t *testing.T) {
	fl, err := parseFlags([]string{"-folders", "1, 2,2", "-workers", "6", "-limit", "9", "-v"})
	if err != nil {
		t.Fatal(err)
	}
	if fl.folderIDs != "1, 2,2" || fl.workers != 6 || fl.limit != 9 || !fl.verbose {
		t.Fatalf("flags = %+v", fl)
	}
}

func TestParseFlagsRejectsUnknown(t *testing.T) {
	if _, err := parseFlags([]string{"-bogus"}); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSplitIDs(t *testing.T) {
	got := splitIDs("0, 2,,2 ,3")
	want := []string{"0", "2", "3"}
	if len(got) != len(want) {
		t.Fatalf("ids = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v", got)
		}
	}
}

func TestApplyFlags(t *testing.T) {
	cfg := config.Default()
	applyFlags(&cfg, cliFlags{outDir: "data", workers: 8, delay: 200 * time.Millisecond})
	if cfg.OutDir != "data" || cfg.Workers != 8 || cfg.DelayMS != 200 {
		t.Fatalf("cfg = %+v", cfg)
	}
}

func TestChooseFoldersPersistsSelection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mexporter.json")
	cfg := config.Default()
	nodes := []*layout.Node{
		{Folder: mailapi.Folder{ID: "0", Name: "Inbox"}, Path: "Inbox"},
		{Folder: mailapi.Folder{ID: "2", Name: "child"}, Path: "Inbox/child"},
	}
	ids, err := chooseFolders(nodes, cfg, cliFlags{configPath: path, folderIDs: "0,2"}, "user@mail_ru_hosted_domain")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %v", ids)
	}
	saved, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Email != "user@mail_ru_hosted_domain" || len(saved.Folders) != 2 || saved.Folders[0] != "0" {
		t.Fatalf("saved = %+v", saved)
	}
}

func TestExitFor(t *testing.T) {
	tests := []struct {
		err  error
		want int
	}{
		{nil, exitFail},
		{fmt.Errorf("wrapped: %w", mailapi.ErrAuth), exitAuth},
		{context.Canceled, exitCanceled},
		{errors.New("other"), exitFail},
	}
	for _, tt := range tests {
		if got := exitFor(tt.err); got != tt.want {
			t.Fatalf("exitFor(%v) = %d, want %d", tt.err, got, tt.want)
		}
	}
}

func TestRunRejectsBadFlag(t *testing.T) {
	if got := run([]string{"-bogus"}); got != exitFail {
		t.Fatalf("exit = %d", got)
	}
}

func TestHumanBytes(t *testing.T) {
	tests := map[int64]string{
		0:       "0 B",
		512:     "512 B",
		1536:    "1.50 KB",
		1 << 20: "1.00 MB",
	}
	for in, want := range tests {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()
	fn()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Close()
	return string(raw)
}

func testNodes() []*layout.Node {
	return []*layout.Node{
		{Folder: mailapi.Folder{ID: "0", Name: "Inbox", MessagesTotal: 1}, Path: "Inbox"},
		{Folder: mailapi.Folder{ID: "2", Name: "child", MessagesTotal: 0}, Path: "Inbox/child"},
	}
}

func TestValidateFolderIDs(t *testing.T) {
	valid, unknown := validateFolderIDs(testNodes(), []string{"0", "2", "2", "9", ""})
	if len(valid) != 2 || valid[0] != "0" || valid[1] != "2" {
		t.Fatalf("valid = %v", valid)
	}
	if len(unknown) != 1 || unknown[0] != "9" {
		t.Fatalf("unknown = %v", unknown)
	}
}

func TestChooseFoldersRejectsUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mexporter.json")
	_, err := chooseFolders(testNodes(), config.Default(), cliFlags{configPath: path, folderIDs: "0,9"}, "user")
	if err == nil || !strings.Contains(err.Error(), "unknown folder") {
		t.Fatalf("err = %v", err)
	}
}

func TestChooseFoldersNoTUIFiltersUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mexporter.json")
	cfg := config.Default()
	cfg.Folders = []string{"0", "9"}
	ids, err := chooseFolders(testNodes(), cfg, cliFlags{configPath: path, noTUI: true}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "0" {
		t.Fatalf("ids = %v", ids)
	}
}

func TestAcquireLock(t *testing.T) {
	dir := t.TempDir()
	release, err := acquireLock(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir, false); err == nil {
		t.Fatal("second lock must fail")
	}
	release()
	if _, err := os.Stat(filepath.Join(dir, ".mexporter.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock still exists: %v", err)
	}
}

func TestAcquireLockForceAndStale(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mexporter.lock")
	if err := os.WriteFile(path, []byte("pid=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireLock(dir, false); err == nil {
		t.Fatal("fresh lock must block")
	}
	release, err := acquireLock(dir, true)
	if err != nil {
		t.Fatalf("force lock: %v", err)
	}
	release()

	if err := os.WriteFile(path, []byte("pid=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	release, err = acquireLock(dir, false)
	if err != nil {
		t.Fatalf("stale lock: %v", err)
	}
	release()
}

func writeVerifyJournal(t *testing.T, accountDir, email string, entries []journal.Entry) {
	t.Helper()
	j, err := journal.Open(accountDir, email)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := j.Record(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunVerifyReportsMismatch(t *testing.T) {
	out := t.TempDir()
	cfgPath := filepath.Join(out, "mexporter.json")
	cfg := config.Default()
	cfg.Email = "user"
	cfg.OutDir = out
	cfg.Folders = []string{"0"}
	if err := config.Save(cfgPath, cfg, time.Now()); err != nil {
		t.Fatal(err)
	}
	accountDir := filepath.Join(out, "user")
	if err := os.MkdirAll(filepath.Join(accountDir, "Inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("Delivered-To: user@example.com\r\nSubject: ok\r\n\r\nbody\r\n")
	if err := os.WriteFile(filepath.Join(accountDir, "Inbox", "m1.eml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	writeVerifyJournal(t, accountDir, "user", []journal.Entry{{
		Folder: "Inbox", FolderID: "0", MessageID: "m1", Status: journal.StatusDownloaded,
		Dest: "Inbox/m1.eml", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]),
	}})
	if got := runVerify(cliFlags{configPath: cfgPath, verifyHash: true}); got != exitOK {
		t.Fatalf("clean verify exit = %d", got)
	}

	if err := os.WriteFile(filepath.Join(accountDir, "Inbox", "m1.eml"), []byte("short"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runVerify(cliFlags{configPath: cfgPath}); got != exitFail {
		t.Fatalf("size mismatch exit = %d", got)
	}

	if err := os.WriteFile(filepath.Join(accountDir, "Inbox", "m1.eml"), data[:len(data)-1], 0o644); err != nil {
		t.Fatal(err)
	}
	// Restore the expected size but change content for the hash check.
	padded := append([]byte{}, data...)
	padded[len(padded)-1] = 'x'
	if err := os.WriteFile(filepath.Join(accountDir, "Inbox", "m1.eml"), padded, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := runVerify(cliFlags{configPath: cfgPath, verifyHash: true}); got != exitFail {
		t.Fatalf("hash mismatch exit = %d", got)
	}

	if err := os.Remove(filepath.Join(accountDir, "Inbox", "m1.eml")); err != nil {
		t.Fatal(err)
	}
	if got := runVerify(cliFlags{configPath: cfgPath}); got != exitFail {
		t.Fatalf("missing file exit = %d", got)
	}
}

func TestPromptSessionFromPastedJSON(t *testing.T) {
	in := strings.NewReader(`
{"email":"user@mail_ru_hosted_domain","access_token":"t","cookies":[{"name":"s","value":"1","domain":".mail.ru"}]}
.
`)
	out := io.Discard
	s, err := promptSession(in, out)
	if err != nil {
		t.Fatal(err)
	}
	if s.Email != "user@mail_ru_hosted_domain" || len(s.Cookies) != 1 {
		t.Fatalf("session = %+v", s)
	}
}

func TestBootstrapSessionRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(path, session.Session{
		Email: "user", AccessToken: "t", Cookies: []session.Cookie{{Name: "s", Value: "1", Domain: ".mail.ru"}},
	}); err != nil {
		t.Fatal(err)
	}
	err := bootstrapSession(cliFlags{sessionPath: path})
	if err == nil || !strings.Contains(err.Error(), "-force") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunRefreshSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/inbox/" && r.URL.Query().Get("wv") == "1":
			_, _ = w.Write([]byte(`<script id="sota.config" type="application/json">` +
				`{"userConfig":{"authCSRFToken":"csrf","api":[{"method":"/api/v1/user/short","data":{"status":"200","email":"refreshed@example.test","body":{"token":"web-token"}}}]}}` +
				`</script>`))
		case r.URL.Path == "/api/v1/threads/status/smart":
			if got := r.URL.Query().Get("token"); got != "web-token" {
				t.Errorf("token = %q", got)
			}
			_, _ = w.Write([]byte(`{"status":200,"email":"refreshed@example.test","body":{}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "session.json")
	if err := session.Save(path, session.Session{
		Email: "old@example.test", Token: "stale",
		Cookies: []session.Cookie{{Name: "s", Value: "1", Domain: ".mail.ru"}},
	}); err != nil {
		t.Fatal(err)
	}
	if code := runRefreshSession(cliFlags{sessionPath: path, baseURL: srv.URL + "/api/v1"}); code != exitOK {
		t.Fatalf("exit = %d", code)
	}
	got, err := session.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != "web-token" || got.Email != "refreshed@example.test" {
		t.Fatalf("session = %+v", got)
	}
}

func TestPrintTreeAndSummaryJSON(t *testing.T) {
	got := captureStdout(t, func() { printTreeJSON(testNodes()) })
	if !strings.Contains(got, `"id": "0"`) || !strings.Contains(got, `"path": "Inbox/child"`) {
		t.Fatalf("tree json = %s", got)
	}
	got = captureStdout(t, func() {
		printSummaryJSON(exporterSummaryForTest(), filepath.Join("out", "user"))
	})
	if !strings.Contains(got, `"account_dir"`) || !strings.Contains(got, `"failed_folders"`) {
		t.Fatalf("summary json = %s", got)
	}
}

func exporterSummaryForTest() exporter.Summary {
	return exporter.Summary{
		Found: 2, Downloaded: 2, Failed: 1, Skipped: 3,
		ThreadsFailed: 1, ThreadsPartial: 2, FailedFolders: 4, Partial: true, Bytes: 10,
	}
}

func TestRunListJSONWithBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/threads/status/smart" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") != "1" {
			t.Errorf("not a folder query: %v", r.URL.Query())
		}
		_, _ = w.Write([]byte(`{"status":"ok","body":{"folders":[{"id":0,"name":"Inbox","messages_total":1}]}}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	sessPath := filepath.Join(dir, "session.json")
	if err := session.Save(sessPath, session.Session{
		Email: "user", AccessToken: "t", Cookies: []session.Cookie{{Name: "s", Value: "1", Domain: ".mail.ru"}},
	}); err != nil {
		t.Fatal(err)
	}
	got := captureStdout(t, func() {
		code := run([]string{"-session", sessPath, "-base-url", srv.URL + "/api/v1", "-list", "-json"})
		if code != exitOK {
			t.Errorf("exit = %d", code)
		}
	})
	if !strings.Contains(got, `"path": "Inbox"`) {
		t.Fatalf("list json = %s", got)
	}
}

func TestRunPartialExportFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") == "1" {
			_, _ = w.Write([]byte(`{"status":"ok","body":{"folders":[{"id":0,"name":"Inbox","messages_total":1}]}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("list failed"))
	}))
	defer srv.Close()

	runCase := func(t *testing.T, allowPartial bool) int {
		t.Helper()
		dir := t.TempDir()
		sessPath := filepath.Join(dir, "session.json")
		if err := session.Save(sessPath, session.Session{
			Email: "user", AccessToken: "t", Cookies: []session.Cookie{{Name: "s", Value: "1", Domain: ".mail.ru"}},
		}); err != nil {
			t.Fatal(err)
		}
		cfgPath := filepath.Join(dir, "mexporter.json")
		cfg := config.Default()
		cfg.Email = "user"
		cfg.OutDir = filepath.Join(dir, "out")
		cfg.Folders = []string{"0"}
		cfg.Workers = 1
		cfg.DelayMS = 1
		if err := config.Save(cfgPath, cfg, time.Now()); err != nil {
			t.Fatal(err)
		}
		args := []string{
			"-session", sessPath, "-config", cfgPath, "-base-url", srv.URL + "/api/v1",
			"-no-tui", "-workers", "1", "-allow-partial",
		}
		if !allowPartial {
			args = args[:len(args)-1]
		}
		return run(args)
	}

	if got := runCase(t, false); got != exitFail {
		t.Fatalf("partial exit = %d", got)
	}
	if got := runCase(t, true); got != exitOK {
		t.Fatalf("allow-partial exit = %d", got)
	}
}
