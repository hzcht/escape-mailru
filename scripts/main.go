package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	imap "github.com/emersion/go-imap"
)

// Config holds the migration configuration.
type Config struct {
	// IMAP server address (e.g. imap.spaceweb.ru)
	Server string `json:"server"`
	// IMAP port (993 for SSL, 143 for plain)
	Port int `json:"port"`
	// Login (email address) for authentication
	Login string `json:"login"`
	// Password for authentication
	Password string `json:"password"`
	// Folder path containing .eml files to migrate
	EMailFolder string `json:"eml_folder"`
}

// DefaultConfig returns a config with reasonable defaults.
func DefaultConfig() Config {
	return Config{
		Server:      "imap.spaceweb.ru",
		Port:        993,
		Login:       "",
		Password:    "",
		EMailFolder: ".",
	}
}

// Validate checks that the config has required fields.
func (c *Config) Validate() error {
	if c.Server == "" {
		return fmt.Errorf("server is required")
	}
	if c.Login == "" {
		return fmt.Errorf("login is required")
	}
	if c.EMailFolder == "" {
		return fmt.Errorf("eml_folder is required")
	}
	return nil
}

// cfgPathExists checks if a file exists and is a regular file.
func cfgPathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// parseArgs attempts to parse config from command-line arguments.
func parseArgs(cfg *Config, args []string) error {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--server":
			if i+1 < len(args) {
				cfg.Server = args[i+1]
				i++
			}
		case "--port":
			if i+1 < len(args) {
				var p int
				fmt.Sscanf(args[i+1], "%d", &p)
				cfg.Port = p
				i++
			}
		case "--login":
			if i+1 < len(args) {
				cfg.Login = args[i+1]
				i++
			}
		case "--password":
			if i+1 < len(args) {
				cfg.Password = args[i+1]
				i++
			}
		case "--eml-folder":
			if i+1 < len(args) {
				cfg.EMailFolder = args[i+1]
				i++
			}
		}
	}
	return nil
}

// main entry point
func main() {
	// Try to load config from file
	configPath := "scripts/config.json"
	cfg := Config{}

	if cfgPathExists(configPath) {
		data, err := ioutil.ReadFile(configPath)
		if err != nil {
			log.Printf("⚠️ Could not read config %s: %v, using CLI args", configPath, err)
		} else {
			if err := json.Unmarshal(data, &cfg); err != nil {
				log.Printf("⚠️ Could not parse config %s: %v", configPath, err)
			} else {
				log.Printf("✅ Loaded config from %s", configPath)
			}
		}
	} else {
		log.Println("ℹ️ No config.json found, using command-line arguments")
		if err := parseArgs(&cfg, os.Args[1:]); err != nil {
			fmt.Fprintf(os.Stderr, "Usage: %s [config.json] or --server --port --login --password --eml-folder\n", os.Args[0])
			os.Exit(1)
		}
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("❌ Invalid config: %v", err)
	}

	log.Println("🚀 Starting migration...")
	log.Printf("Server: %s:%d", cfg.Server, cfg.Port)
	log.Printf("Login: %s", cfg.Login)
	log.Printf("EMail folder: %s", cfg.EMailFolder)

	if err := Migrate(cfg); err != nil {
		log.Fatalf("❌ Migration failed: %v", err)
	}

	log.Println("🎉 Migration completed successfully!")
}

// Migrate migrates .eml files from a local folder to an IMAP server.
func Migrate(cfg Config) error {
	log.Printf("Starting migration to IMAP server %s:%d", cfg.Server, cfg.Port)

	// Connect to IMAP server
	var client *imap.Client
	if cfg.Port == 993 {
		// Use TLS
		tlsConfig := &tls.Config{ServerName: cfg.Server}
		c, err := imap.DialTLS(cfg.Server, cfg.Port, tlsConfig)
		if err != nil {
			return fmt.Errorf("dial failed: %w", err)
		}
		client = c
	} else {
		c, err := imap.Dial(cfg.Server + ":" + fmt.Sprintf("%d", cfg.Port))
		if err != nil {
			return fmt.Errorf("dial failed: %w", err)
		}
		client = c
	}

	if err := client.Login(cfg.Login, cfg.Password); err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	log.Println("Logged in successfully")

	// Select mailbox (INBOX by default)
	mailbox := "INBOX"
	mbox, err := client.Select(mailbox, false)
	if err != nil {
		return fmt.Errorf("select mailbox failed: %w", err)
	}
	log.Printf("Selected mailbox [%s] (%d existing messages)", mailbox, mbox.Messages)

	// Walk the EMl folder for .eml files
	emlFiles, err := walkForEML(cfg.EMailFolder)
	if err != nil {
		return fmt.Errorf("walk folder failed: %w", err)
	}
	log.Printf("Found %d .eml files to migrate", len(emlFiles))

	// Process each .eml file
	successCount := 0
	errorCount := 0

	for i, filePath := range emlFiles {
		log.Printf("Processing %d/%d: %s", i, len(emlFiles), filepath.Base(filePath))

		// Read the .eml file raw
		rawData, err := ioutil.ReadFile(filePath)
		if err != nil {
			log.Printf("⚠️ Failed to read %s: %v", filepath.Base(filePath), err)
			errorCount++
			continue // Skip error, continue with next
		}

		// Append to mailbox
		if err := client.Append(mailbox, nil, nil, rawData); err != nil {
			log.Printf("❌ Failed to append %s: %v", filepath.Base(filePath), err)
			errorCount++
			continue // Skip error, continue with next
		}

		successCount++
		log.Printf("✅ Successfully migrated: %s", filepath.Base(filePath))
	}

	log.Printf("Migration complete: %d succeeded, %d failed", successCount, errorCount)

	// Close connection
	client.Logout()
	log.Println("IMAP connection closed")

	return nil
}

// walkForEML recursively finds .eml files in the given directory.
func walkForEML(dir string) ([]string, error) {
	var files []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			log.Printf("⚠️ Walk error accessing %s: %v", path, err)
			return nil // skip this directory/file but continue
		}
		if !info.IsDir() && strings.ToLower(filepath.Ext(path)) == ".eml" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}
