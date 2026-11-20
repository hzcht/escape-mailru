// Package config stores the operator's folder selection and run defaults.
//
// REQ-EXPORT-006: the selection survives restarts and is saved before an export starts.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// Version is the current config schema version.
	Version = 1
	// DefaultOutDir is used when no output directory is configured.
	DefaultOutDir = "mail-export"
	// DefaultWorkers is the default download concurrency.
	DefaultWorkers = 4
	// DefaultDelayMS is the default pause per worker between downloads.
	DefaultDelayMS = 150
)

// Config is the persisted operator configuration.
type Config struct {
	Version   int      `json:"version"`
	Email     string   `json:"email"`
	OutDir    string   `json:"out_dir"`
	Workers   int      `json:"workers"`
	DelayMS   int      `json:"delay_ms"`
	Folders   []string `json:"folders"`
	UpdatedAt string   `json:"updated_at"`
}

// Default returns a usable configuration.
func Default() Config {
	return Config{
		Version: Version,
		OutDir:  DefaultOutDir,
		Workers: DefaultWorkers,
		DelayMS: DefaultDelayMS,
	}
}

// Load reads a config file; a missing file returns Default.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil
		}
		return Config{}, fmt.Errorf("REQ-EXPORT-006: read config: %w", err)
	}
	cfg := Default()
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("REQ-EXPORT-006: parse config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}

// Normalize fills defaults and clamps unsafe values.
func (c *Config) Normalize() {
	if c.Version == 0 {
		c.Version = Version
	}
	if c.OutDir == "" {
		c.OutDir = DefaultOutDir
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Workers > 32 {
		c.Workers = 32
	}
	if c.DelayMS <= 0 {
		c.DelayMS = DefaultDelayMS
	}
	if c.DelayMS > 60_000 {
		c.DelayMS = 60_000
	}
}

// Save writes the config atomically and stamps UpdatedAt.
func Save(path string, c Config, now time.Time) error {
	c.Normalize()
	c.Version = Version
	c.UpdatedAt = now.Format(time.RFC3339)
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-006: encode config: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("REQ-EXPORT-006: create config dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".mexporter-*.tmp")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-006: create config temp: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-006: write config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-006: close config: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("REQ-EXPORT-006: replace config: %w", err)
	}
	return nil
}
