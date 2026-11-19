package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"mexporter/internal/config"
	"mexporter/internal/mailapi"
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
	ids, err := chooseFolders(nil, cfg, cliFlags{configPath: path, folderIDs: "0,2"}, "user@mail_ru_hosted_domain")
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
