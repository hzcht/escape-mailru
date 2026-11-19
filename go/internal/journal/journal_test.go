package journal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
