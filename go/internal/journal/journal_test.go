package journal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordStatsAndResume(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "user@mail_ru_hosted_domain")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	entries := []Entry{
		{Folder: "Входящие", FolderID: "0", MessageID: "m1", Subject: "one", Status: StatusDownloaded, Size: 100, Dest: "a.eml"},
		{Folder: "Входящие", FolderID: "0", MessageID: "m2", Subject: "two", Status: StatusFailed, Error: "boom"},
		{Folder: "Входящие", FolderID: "0", MessageID: "m3", Status: StatusPending},
	}
	for _, e := range entries {
		if err := j.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	stats := j.Stats()
	if stats.Total != 3 || stats.Downloaded != 1 || stats.Failed != 1 || stats.Pending != 1 || stats.Bytes != 100 {
		t.Fatalf("stats = %+v", stats)
	}
	if e, ok := j.Status("m2"); !ok || e.Error != "boom" {
		t.Fatalf("status = %+v %v", e, ok)
	}
	if _, ok := j.Status("missing"); ok {
		t.Fatal("unexpected entry")
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, snapshotName)); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, jsonlName))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("jsonl is empty")
	}

	reopened, err := Open(dir, "user@mail_ru_hosted_domain")
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got := reopened.Stats(); got.Total != 3 || got.Downloaded != 1 {
		t.Fatalf("resumed stats = %+v", got)
	}
}

func TestLoadSnapshotFallback(t *testing.T) {
	dir := t.TempDir()
	snap := Snapshot{Version: 1, Email: "x", Entries: []Entry{
		{MessageID: "m1", Status: StatusDownloaded, Size: 7},
		{MessageID: "", Status: StatusFailed},
	}}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := j.Stats(); got.Total != 1 || got.Downloaded != 1 {
		t.Fatalf("snapshot stats = %+v", got)
	}
}

func TestScanSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	content := "{\"id\":\"m1\",\"status\":\"downloaded\"}\nnot json\n\n{\"id\":\"m2\",\"status\":\"failed\"}\n"
	if err := os.WriteFile(filepath.Join(dir, jsonlName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := j.Stats(); got.Total != 2 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestRecordCanceled(t *testing.T) {
	j, err := Open(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := j.Record(ctx, Entry{MessageID: "m1"}); err == nil {
		t.Fatal("expected context error")
	}
	if got := j.Stats(); got.Total != 0 {
		t.Fatalf("entry recorded: %+v", got)
	}
	if err := j.Save(ctx); err == nil {
		t.Fatal("expected save context error")
	}
}

func TestCloseReleasesFileAfterSnapshotError(t *testing.T) {
	j, err := Open(t.TempDir(), "user@mail_ru_hosted_domain")
	if err != nil {
		t.Fatal(err)
	}
	j.dir = filepath.Join(t.TempDir(), "missing")
	if err := j.Close(); err == nil {
		t.Fatal("expected snapshot error")
	}
	if _, err := j.jsonl.Stat(); err == nil {
		t.Fatal("jsonl must be closed even when the snapshot fails")
	}
}

func TestOpenBadSnapshot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, snapshotName), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, "x"); err == nil {
		t.Fatal("expected snapshot parse error")
	}
}

func writeSnapshot(t *testing.T, dir string, entries ...Entry) {
	t.Helper()
	raw, err := json.Marshal(Snapshot{Version: 1, Email: "x", Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSnapshotThenJSONL(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir,
		Entry{MessageID: "m1", Status: StatusDownloaded, Size: 1},
		Entry{MessageID: "m2", Status: StatusDownloaded, Size: 2},
	)
	content := "{\"id\":\"m2\",\"status\":\"failed\"}\n{\"id\":\"m3\",\"status\":\"downloaded\",\"size\":3}\n"
	if err := os.WriteFile(filepath.Join(dir, jsonlName), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	stats := j.Stats()
	if stats.Total != 3 || stats.Downloaded != 2 || stats.Failed != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	if e, _ := j.Status("m2"); e.Status != StatusFailed {
		t.Fatalf("jsonl must win: %+v", e)
	}
}

func TestEmptyJSONLKeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, Entry{MessageID: "m1", Status: StatusDownloaded, Size: 1})
	if err := os.WriteFile(filepath.Join(dir, jsonlName), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := j.Stats(); got.Total != 1 || got.Downloaded != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestCorruptJSONLKeepsSnapshot(t *testing.T) {
	dir := t.TempDir()
	writeSnapshot(t, dir, Entry{MessageID: "m1", Status: StatusDownloaded, Size: 1})
	if err := os.WriteFile(filepath.Join(dir, jsonlName), []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	j, err := Open(dir, "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if got := j.Stats(); got.Total != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestVerify(t *testing.T) {
	account := t.TempDir()
	content := []byte("message bytes")
	if err := os.MkdirAll(filepath.Join(account, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(account, "sub", "a.eml"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	j, err := Open(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	for _, e := range []Entry{
		{MessageID: "good", Status: StatusDownloaded, Dest: "sub/a.eml", Size: int64(len(content)), SHA256: hash},
		{MessageID: "missing", Status: StatusDownloaded, Dest: "sub/b.eml", Size: 1},
		{MessageID: "size", Status: StatusDownloaded, Dest: "sub/a.eml", Size: int64(len(content)) + 1},
		{MessageID: "unsafe", Status: StatusDownloaded, Dest: "../evil.eml", Size: 1},
		{MessageID: "nohash", Status: StatusDownloaded, Dest: "sub/a.eml", Size: int64(len(content))},
	} {
		if err := j.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := j.Verify(account, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 5 || rep.OK != 2 || rep.Missing != 1 || rep.SizeMismatch != 1 || rep.Unsafe != 1 || rep.NoHash != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Issues) != 3 {
		t.Fatalf("issues = %+v", rep.Issues)
	}
}

func TestOpenReadOnlyDoesNotModify(t *testing.T) {
	dir := t.TempDir()
	j, err := Open(dir, "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Record(context.Background(), Entry{MessageID: "m1", Status: StatusDownloaded, Dest: "a.eml", Size: 10}); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(dir, "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := ro.Stats().Total; got != 1 {
		t.Fatalf("total = %d, want 1", got)
	}
	if len(ro.Entries()) != 1 {
		t.Fatalf("entries = %d, want 1", len(ro.Entries()))
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("file count changed: %d -> %d", len(before), len(after))
	}
}

func TestOpenReadOnlyEmptyDir(t *testing.T) {
	dir := t.TempDir()
	ro, err := OpenReadOnly(dir, "user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got := ro.Stats().Total; got != 0 {
		t.Fatalf("total = %d, want 0", got)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("read-only open created files: %v", files)
	}
}

func TestVerifyHashEmptyAndUnreadable(t *testing.T) {
	account := t.TempDir()
	if err := os.WriteFile(filepath.Join(account, "a.eml"), []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(account, "empty.eml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(account, "dir.eml"), 0o755); err != nil {
		t.Fatal(err)
	}
	j, err := Open(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	for _, e := range []Entry{
		{MessageID: "hash", Status: StatusDownloaded, Dest: "a.eml", Size: 3, SHA256: strings.Repeat("00", 32)},
		{MessageID: "empty", Status: StatusDownloaded, Dest: "empty.eml", Size: 0},
		{MessageID: "dir", Status: StatusDownloaded, Dest: "dir.eml", Size: 1},
		{MessageID: "nopath", Status: StatusDownloaded, Dest: "   ", Size: 1},
	} {
		if err := j.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := j.Verify(account, true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 4 || rep.OK != 0 || rep.HashMismatch != 1 || rep.SizeMismatch != 1 ||
		rep.Unreadable != 1 || rep.Missing != 1 || rep.NoHash != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if len(rep.Issues) != 4 {
		t.Fatalf("issues = %+v", rep.Issues)
	}
}

func TestVerifyIssueLimit(t *testing.T) {
	j, err := Open(t.TempDir(), "x")
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	ctx := context.Background()
	for i := 0; i < verifyIssueLimit+5; i++ {
		e := Entry{MessageID: fmt.Sprintf("m%03d", i), Status: StatusDownloaded, Dest: fmt.Sprintf("gone%03d.eml", i), Size: 1}
		if err := j.Record(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := j.Verify(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Missing != verifyIssueLimit+5 || len(rep.Issues) != verifyIssueLimit || !rep.Truncated {
		t.Fatalf("report = %+v", rep)
	}
}
