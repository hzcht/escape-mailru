package exporter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mexporter/internal/journal"
	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

const validEml = "Delivered-To: user@mail_ru_hosted_domain\r\nSubject: test\r\n\r\n" + "body-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\r\n"

type fakeAPI struct {
	pages       map[string]map[int][]mailapi.Thread
	pinned      map[string][]mailapi.Thread
	threads     map[string][]mailapi.Message
	threadErr   map[string]error
	data        map[string][]byte
	downloadErr map[string]error
	listErr     map[string]error
}

func newFake() *fakeAPI {
	return &fakeAPI{
		pages:       map[string]map[int][]mailapi.Thread{},
		pinned:      map[string][]mailapi.Thread{},
		threads:     map[string][]mailapi.Message{},
		threadErr:   map[string]error{},
		data:        map[string][]byte{},
		downloadErr: map[string]error{},
		listErr:     map[string]error{},
	}
}

func (f *fakeAPI) ListPage(_ context.Context, folderID string, offset, _ int, _ mailapi.Order) (*mailapi.ListBody, error) {
	if err := f.listErr[folderID]; err != nil {
		return nil, err
	}
	body := &mailapi.ListBody{Threads: f.pages[folderID][offset]}
	if offset == 0 {
		body.Pinned.Messages = f.pinned[folderID]
	}
	return body, nil
}

func (f *fakeAPI) ThreadMessages(_ context.Context, threadID string) ([]mailapi.Message, error) {
	if err := f.threadErr[threadID]; err != nil {
		return nil, err
	}
	return f.threads[threadID], nil
}

func (f *fakeAPI) DownloadRaw(_ context.Context, messageID, _ string) ([]byte, error) {
	if err := f.downloadErr[messageID]; err != nil {
		return nil, err
	}
	data, ok := f.data[messageID]
	if !ok {
		return nil, errors.New("missing fake data")
	}
	return data, nil
}

func node(id, path string, total int) *layout.Node {
	return &layout.Node{
		Folder: mailapi.Folder{ID: mailapi.ID(id), Name: path, MessagesTotal: total},
		Path:   path,
	}
}

func twoMessages() *fakeAPI {
	f := newFake()
	f.pages["0"] = map[int][]mailapi.Thread{
		0: {
			{ID: "t1", Length: 1, Last: "m1", Folder: "0", Subject: "one", Date: 100},
			{ID: "t2", Length: 1, Last: "m2", Folder: "0", Subject: "two", Date: 200},
		},
	}
	f.data["m1"] = []byte(validEml)
	f.data["m2"] = []byte(validEml)
	return f
}

func TestRunDownloadsAndResumes(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "user@mail_ru_hosted_domain")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := twoMessages()
	s := New(f, j, Options{AccountDir: account, Workers: 2, PageSize: 2, Delay: 0})
	nodes := []*layout.Node{node("0", "Inbox", 2)}
	sum, err := s.Run(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Found != 2 || sum.Downloaded != 2 || sum.Failed != 0 || sum.Bytes != int64(2*len(validEml)) {
		t.Fatalf("summary = %+v", sum)
	}
	files, err := filepath.Glob(filepath.Join(account, "Inbox", "*.eml"))
	if err != nil || len(files) != 2 {
		t.Fatalf("files = %v err=%v", files, err)
	}
	if got := j.Stats(); got.Downloaded != 2 {
		t.Fatalf("journal = %+v", got)
	}

	s2 := New(f, j, Options{AccountDir: account, Workers: 2, PageSize: 2})
	sum, err = s2.Run(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Found != 2 || sum.Skipped != 2 || sum.Downloaded != 0 {
		t.Fatalf("resume summary = %+v", sum)
	}
}

func TestRunDryRun(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	s := New(twoMessages(), j, Options{AccountDir: account, Workers: 1, PageSize: 2, DryRun: true})
	sum, err := s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 2)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Found != 2 || sum.Downloaded != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if files, _ := filepath.Glob(filepath.Join(account, "Inbox", "*.eml")); len(files) != 0 {
		t.Fatalf("dry run wrote %d files", len(files))
	}
}

func TestMultiThreadExpansionAndFallback(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := newFake()
	f.pages["0"] = map[int][]mailapi.Thread{
		0: {
			{ID: "2:abc:0", Length: 2, Last: "m2", Folder: "0", Subject: "thread"},
			{ID: "2:def:0", Length: 2, Last: "m4", Folder: "0", Subject: "broken"},
		},
	}
	f.threads["2:abc:0"] = []mailapi.Message{{ID: "m1"}, {ID: "m2"}}
	f.threadErr["2:def:0"] = errors.New("boom")
	f.data["m1"] = []byte(validEml)
	f.data["m2"] = []byte(validEml)
	f.data["m4"] = []byte(validEml)
	s := New(f, j, Options{AccountDir: account, Workers: 1, PageSize: 2})
	sum, err := s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 3)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Found != 3 || sum.Downloaded != 3 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestLimitStopsDownloads(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := twoMessages()
	s := New(f, j, Options{AccountDir: account, Workers: 1, PageSize: 2, Limit: 1})
	sum, err := s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 2)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Downloaded != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if _, ok := j.Status("m2"); ok {
		t.Fatal("limit must stop further downloads")
	}
}

func TestAuthErrorStopsRun(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := twoMessages()
	f.downloadErr["m1"] = mailapi.ErrAuth
	s := New(f, j, Options{AccountDir: account, Workers: 1, PageSize: 2})
	_, err = s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 2)})
	if !errors.Is(err, mailapi.ErrAuth) {
		t.Fatalf("err = %v", err)
	}
}

func TestFolderListErrorContinues(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := twoMessages()
	f.listErr["1"] = errors.New("boom")
	s := New(f, j, Options{AccountDir: account, Workers: 1, PageSize: 2, DryRun: true})
	nodes := []*layout.Node{node("1", "Broken", 1), node("0", "Inbox", 2)}
	sum, err := s.Run(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Found != 2 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestWriteManifest(t *testing.T) {
	dir := t.TempDir()
	nodes := []*layout.Node{node("0", "Inbox", 5), node("2", "Inbox/booking", 0)}
	if err := WriteManifest(dir, nodes); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "folders.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recs []manifestRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1].Path != "Inbox/booking" || recs[0].Messages != 5 {
		t.Fatalf("records = %+v", recs)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("abcdef", 4); got != "abc…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncateRunes("abc", 4); got != "abc" {
		t.Fatalf("short = %q", got)
	}
	if got := truncateRunes("abc", 0); got != "" {
		t.Fatalf("zero = %q", got)
	}
	if got := truncateRunes(longRussian(), 5); !strings.HasSuffix(got, "…") {
		t.Fatalf("russian = %q", got)
	}
}

func longRussian() string { return strings.Repeat("я", 20) }

func TestFormatDate(t *testing.T) {
	if got := formatDate(0); got != "" {
		t.Fatalf("zero = %q", got)
	}
	if got := formatDate(1700000000); !strings.Contains(got, "T") {
		t.Fatalf("date = %q", got)
	}
}

func TestNewDefaultsAndSelection(t *testing.T) {
	s := New(newFake(), nil, Options{})
	if s.opts.Workers != 1 || s.opts.PageSize != 50 {
		t.Fatalf("defaults = %+v", s.opts)
	}
	if s.selectedSet() != nil {
		t.Fatal("empty selection must mean all folders")
	}
	s.opts.Folders = []string{"", "2", "3"}
	set := s.selectedSet()
	if len(set) != 2 || !set["2"] || !set["3"] {
		t.Fatalf("selection = %v", set)
	}
}

func TestLogf(t *testing.T) {
	New(newFake(), nil, Options{}).logf("no logger %d", 1)
	var buf bytes.Buffer
	s := New(newFake(), nil, Options{Logf: func(f string, a ...any) { _, _ = fmt.Fprintf(&buf, f, a...) }})
	s.logf("value=%d", 7)
	if buf.String() != "value=7" {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestRecordFailureThroughRun(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	f := twoMessages()
	f.downloadErr["m1"] = context.Canceled
	f.downloadErr["m2"] = context.Canceled
	s := New(f, j, Options{AccountDir: account, Workers: 1, PageSize: 2})
	sum, err := s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 2)})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 2 || sum.Downloaded != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if got := j.Stats(); got.Failed != 2 {
		t.Fatalf("journal = %+v", got)
	}
}

func TestFetchRateLimitedHonoursContext(t *testing.T) {
	f := newFake()
	f.downloadErr["m1"] = mailapi.ErrRateLimited
	s := New(f, nil, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	defer cancel()
	_, err := s.fetch(ctx, job{messageID: "m1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestDownloadJobWriteError(t *testing.T) {
	f := newFake()
	f.data["m1"] = []byte(validEml)
	s := New(f, nil, Options{AccountDir: t.TempDir()})
	rel, size, err := s.downloadJob(context.Background(), job{messageID: "m1", folderPath: "missing"})
	if err == nil || size != 0 || rel != "missing/no-date_m1.eml" {
		t.Fatalf("rel=%q size=%d err=%v", rel, size, err)
	}
}

func TestWriteFileAtomicMissingDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "x.eml")
	if err := writeFileAtomic(path, []byte("data")); err == nil {
		t.Fatal("expected error")
	}
}

func TestProgressLoop(t *testing.T) {
	old := progressInterval
	progressInterval = 5 * time.Millisecond
	defer func() { progressInterval = old }()

	var buf bytes.Buffer
	s := New(newFake(), nil, Options{Progress: &buf})
	s.downloaded.Store(3)
	s.failed.Store(1)
	s.skipped.Store(2)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.progressLoop(context.Background(), stop)
	}()
	time.Sleep(25 * time.Millisecond)
	close(stop)
	<-done
	if !strings.Contains(buf.String(), "ok=3 fail=1 skip=2") {
		t.Fatalf("progress = %q", buf.String())
	}
}

func TestVerboseLogsDownloads(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	var buf bytes.Buffer
	logf := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&buf, format, args...)
		buf.WriteByte('\n')
	}
	s := New(twoMessages(), j, Options{AccountDir: account, Workers: 1, PageSize: 2, Verbose: true, Logf: logf})
	if _, err := s.Run(context.Background(), []*layout.Node{node("0", "Inbox", 2)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "OK m1") || !strings.Contains(buf.String(), "OK m2") {
		t.Fatalf("log = %q", buf.String())
	}
}

func TestRunCanceledContext(t *testing.T) {
	account := t.TempDir()
	j, err := journal.Open(filepath.Join(account, "journal"), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = New(twoMessages(), j, Options{AccountDir: account}).Run(ctx, []*layout.Node{node("0", "Inbox", 2)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}
