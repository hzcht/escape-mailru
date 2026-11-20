package layout

import (
	"strings"
	"testing"
	"time"

	"mexporter/internal/mailapi"
)

func TestBuildTreeNestedDuplicatesAndOrphans(t *testing.T) {
	folders := []mailapi.Folder{
		{ID: "0", Name: "Входящие"},
		{ID: "2", Name: "booking", Parent: "0"},
		{ID: "23", Name: "Рассылки", Parent: "0"},
		{ID: "500013", Name: "Рассылки", Parent: "0"},
		{ID: "4", Name: "folder@mail_ru_hosted_domain"},
		{ID: "5", Name: "APPLE", Parent: "4"},
		{ID: "99", Name: "Orphan", Parent: "404"},
		{ID: "0", Name: "duplicate id"},
	}
	roots, byID := BuildTree(folders)
	if len(byID) != 7 {
		t.Fatalf("byID = %d, want 7", len(byID))
	}
	flat := Flatten(roots)
	if len(flat) != 7 {
		t.Fatalf("flatten = %d nodes, want 7", len(flat))
	}
	if flat[0].Path != "Входящие" || flat[0].Depth != 0 {
		t.Fatalf("root = %q depth %d", flat[0].Path, flat[0].Depth)
	}
	if got := byID["2"].Path; got != "Входящие/booking" {
		t.Fatalf("booking path = %q", got)
	}
	if got := byID["23"].Path; got != "Входящие/Рассылки (23)" {
		t.Fatalf("first duplicate path = %q", got)
	}
	if got := byID["500013"].Path; got != "Входящие/Рассылки (500013)" {
		t.Fatalf("second duplicate path = %q", got)
	}
	if got := byID["5"].Path; got != "folder@mail_ru_hosted_domain/APPLE" {
		t.Fatalf("nested path = %q", got)
	}
	if got := byID["99"].Path; got != "Orphan" {
		t.Fatalf("orphan path = %q", got)
	}
	if byID["5"].Depth != 1 {
		t.Fatalf("nested depth = %d", byID["5"].Depth)
	}
}

func TestBuildTreeSkipsEmptyIDs(t *testing.T) {
	roots, byID := BuildTree([]mailapi.Folder{{Name: "no id"}, {ID: "", Name: "empty"}})
	if len(roots) != 0 || len(byID) != 0 {
		t.Fatalf("roots=%d byID=%d, want empty", len(roots), len(byID))
	}
	if got := Flatten(nil); len(got) != 0 {
		t.Fatalf("flatten nil = %d", len(got))
	}
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		in   string
		max  int
		want string
	}{
		{"a<b>c:d\"e/f\\g|h?i*j", 0, "a_b_c_d_e_f_g_h_i_j"},
		{"  many   spaces\tand\nlines  ", 0, "many spaces_and_lines"},
		{"trailing...   ", 0, "trailing"},
		{"", 0, "_"},
		{"...", 0, "_"},
		{"CON", 0, "_CON"},
		{"con.txt", 0, "_con.txt"},
		{"abcdef", 3, "abc"},
		{"a...", 2, "a"},
		{"x\x01y", 0, "x_y"},
	}
	for _, tt := range tests {
		if got := SanitizeName(tt.in, tt.max); got != tt.want {
			t.Errorf("SanitizeName(%q, %d) = %q, want %q", tt.in, tt.max, got, tt.want)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	ts := time.Date(2024, time.March, 5, 12, 0, 0, 0, time.Local).Unix()
	got := SafeFilename(ts, "Отчёт: март/2024", "17950342870824228787")
	want := "2024-03-05_Отчёт_ март_2024_17950342870824228787.eml"
	if got != want {
		t.Fatalf("SafeFilename = %q, want %q", got, want)
	}
	if got := SafeFilename(0, "", "42"); got != "no-date_42.eml" {
		t.Fatalf("empty filename = %q", got)
	}
	long := strings.Repeat("я", 200)
	name := SafeFilename(ts, long, "1")
	if n := len([]rune(strings.TrimSuffix(name, ".eml"))); n != 63 {
		t.Fatalf("long name length = %d, want 63", n)
	}
	if !strings.HasSuffix(name, "_1.eml") {
		t.Fatalf("uidl suffix lost: %q", name)
	}
}

func TestSafeFilenameKeepsUniqueLongUIDL(t *testing.T) {
	ts := time.Date(2024, time.March, 5, 12, 0, 0, 0, time.Local).Unix()
	base := strings.Repeat("a", 200)
	first := SafeFilename(ts, "subject", base+"1")
	second := SafeFilename(ts, "subject", base+"2")
	if first == second {
		t.Fatalf("long uids collide: %q", first)
	}
	if !strings.HasSuffix(first, ".eml") || !strings.HasSuffix(second, ".eml") {
		t.Fatalf("missing .eml suffix: %q %q", first, second)
	}
	if strings.ContainsAny(first, `/\:*?"<>|`) {
		t.Fatalf("unsafe filename: %q", first)
	}
}

func TestSafeFilenameSanitizesUnsafeUIDL(t *testing.T) {
	got := SafeFilename(0, "", `../a\b:c`)
	if strings.ContainsAny(got, `/\:`) {
		t.Fatalf("unsafe uidl survived: %q", got)
	}
	if got == SafeFilename(0, "", "a_b_c") {
		t.Fatalf("unsafe uidl must keep an identity hash: %q", got)
	}
}
