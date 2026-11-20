package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefault(t *testing.T) {
	c := Default()
	if c.Version != Version || c.OutDir != DefaultOutDir || c.Workers != DefaultWorkers || c.DelayMS != DefaultDelayMS {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadMissingReturnsDefault(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.OutDir != DefaultOutDir || c.Workers != DefaultWorkers {
		t.Fatalf("defaults not applied: %+v", c)
	}
}

func TestLoadInvalidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	now := time.Date(2026, 11, 19, 10, 30, 0, 0, time.UTC)
	c := Config{Email: "user@mail_ru_hosted_domain", OutDir: "out", Workers: 7, DelayMS: 42, Folders: []string{"0", "2"}}
	if err := Save(path, c, now); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var saved Config
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Email != c.Email || saved.UpdatedAt != now.Format(time.RFC3339) {
		t.Fatalf("saved = %+v", saved)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Workers != 7 || loaded.DelayMS != 42 || len(loaded.Folders) != 2 {
		t.Fatalf("loaded = %+v", loaded)
	}
}

func TestNormalizeClamps(t *testing.T) {
	c := Config{}
	c.Normalize()
	if c.Version != Version || c.OutDir != DefaultOutDir || c.Workers != DefaultWorkers || c.DelayMS != DefaultDelayMS {
		t.Fatalf("defaults: %+v", c)
	}
	c = Config{Workers: 99, DelayMS: 999999}
	c.Normalize()
	if c.Workers != 32 || c.DelayMS != 60000 {
		t.Fatalf("upper clamps: %+v", c)
	}
}

func TestSaveToDirectoryFails(t *testing.T) {
	if err := Save(t.TempDir(), Default(), time.Now()); err == nil {
		t.Fatal("expected replace error")
	}
}

func TestSaveBadDirectory(t *testing.T) {
	err := Save(filepath.Join(t.TempDir(), "missing", "config.json"), Default(), time.Now())
	if err == nil {
		t.Fatal("expected create temp error")
	}
}
