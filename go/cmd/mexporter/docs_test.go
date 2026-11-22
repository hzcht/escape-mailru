package main

import (
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoFile reads a file from the repository root. Tests run in the package
// directory, so the root is three levels up from go/cmd/mexporter.
func repoFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// TestReadmeDocumentsFlags keeps the flag table in README.md in sync with the
// real command line surface: every defined flag must be named there.
func TestReadmeDocumentsFlags(t *testing.T) {
	readme := repoFile(t, "README.md")
	var fl cliFlags
	newFlagSet(&fl).VisitAll(func(f *flag.Flag) {
		if !strings.Contains(readme, "`-"+f.Name+"`") {
			t.Errorf("flag -%s is missing from README.md", f.Name)
		}
	})
}

var pyLongOption = regexp.MustCompile(`"(--[a-z][a-z-]*)"`)

// TestScriptsReadmeDocumentsArguments keeps scripts/README.md in sync with the
// migration script: every long option it accepts must be documented.
func TestScriptsReadmeDocumentsArguments(t *testing.T) {
	script := repoFile(t, filepath.Join("scripts", "migrate_to_imap.py"))
	doc := repoFile(t, filepath.Join("scripts", "README.md"))
	seen := make(map[string]bool)
	for _, m := range pyLongOption.FindAllStringSubmatch(script, -1) {
		if seen[m[1]] {
			continue
		}
		seen[m[1]] = true
		if !strings.Contains(doc, "`"+m[1]+"`") {
			t.Errorf("argument %s is missing from scripts/README.md", m[1])
		}
	}
	// Guard against a regex that silently matches nothing.
	if len(seen) < 10 {
		t.Fatalf("found only %d long options in the script; the parser regex is probably broken", len(seen))
	}
}
