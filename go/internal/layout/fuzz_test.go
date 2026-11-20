package layout

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// unsafeNameChars are the characters Windows forbids in file names.
const unsafeNameChars = "<>:\"/\\|?*"

// assertSafeComponent checks the invariants every generated path component
// must hold: non-empty, valid UTF-8, no forbidden or control characters, no
// leading/trailing dot or space, and never a Windows device name.
func assertSafeComponent(t *testing.T, got string) {
	t.Helper()
	if got == "" {
		t.Fatal("component is empty")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("component is not valid UTF-8: %q", got)
	}
	if strings.ContainsAny(got, unsafeNameChars) {
		t.Fatalf("component contains unsafe characters: %q", got)
	}
	for _, r := range got {
		if r < 32 {
			t.Fatalf("component contains control rune U+%04X: %q", r, got)
		}
	}
	if strings.Trim(got, " .") != got {
		t.Fatalf("component has a leading or trailing dot/space: %q", got)
	}
	if got == "." || got == ".." {
		t.Fatalf("component is a traversal entry: %q", got)
	}
	base := got
	if i := strings.IndexByte(strings.ToUpper(base), '.'); i > 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToUpper(base)] {
		t.Fatalf("component is a reserved device name: %q", got)
	}
}

func FuzzSanitizeName(f *testing.F) {
	f.Add("Inbox", 64)
	f.Add("CON", 0)
	f.Add("..", 0)
	f.Add("a/b\\c:d", 10)
	f.Add("", 0)
	f.Add("  spaced   name  ", 0)
	f.Add("Рассылки (23)", 0)
	f.Fuzz(func(t *testing.T, name string, maxRunes int) {
		if maxRunes < 0 || maxRunes > 1<<20 {
			t.Skip("unrealistic limit")
		}
		got := SanitizeName(name, maxRunes)
		assertSafeComponent(t, got)
		if maxRunes > 1 {
			// The reserved-name guard may prepend one rune.
			if n := len([]rune(got)); n > maxRunes+1 {
				t.Fatalf("length %d exceeds limit %d: %q", n, maxRunes, got)
			}
		}
	})
}

func FuzzSafeFilename(f *testing.F) {
	f.Add(int64(1700000000), "Meeting notes", "uid-123")
	f.Add(int64(0), "", "")
	f.Add(int64(-5), "a/b\\c:d", "..")
	f.Add(int64(1<<40), strings.Repeat("с", 200), strings.Repeat("u", 500))
	f.Fuzz(func(t *testing.T, dateUnix int64, subject, uidl string) {
		got := SafeFilename(dateUnix, subject, uidl)
		if !strings.HasSuffix(got, ".eml") {
			t.Fatalf("missing .eml suffix: %q", got)
		}
		assertSafeComponent(t, got)
		if again := SafeFilename(dateUnix, subject, uidl); again != got {
			t.Fatalf("not deterministic: %q then %q", got, again)
		}
	})
}

// FuzzSafeFilenameIdentity pins the identity rule: two readable, short uidls
// must never collapse into the same file name, whatever the subject is.
func FuzzSafeFilenameIdentity(f *testing.F) {
	f.Add(int64(1700000000), "subject", "uid-one", "uid-two")
	f.Add(int64(0), "", "a", "b")
	f.Add(int64(1), "субъект", "1", "2")
	f.Fuzz(func(t *testing.T, dateUnix int64, subject, a, b string) {
		if a == b || !readableUIDL(a) || !readableUIDL(b) {
			t.Skip("needs two distinct readable uidls")
		}
		na := SafeFilename(dateUnix, subject, a)
		nb := SafeFilename(dateUnix, subject, b)
		if na == nb {
			t.Fatalf("distinct uidls collapsed into %q: %q and %q", na, a, b)
		}
	})
}

// readableUIDL reports whether uidl survives sanitisation unchanged and stays
// short enough to be used verbatim in a file name.
func readableUIDL(uidl string) bool {
	return uidl != "" && uidl == SanitizeName(uidl, 0) && len([]rune(uidl)) <= 80
}
