package tui

import (
	"strings"
	"testing"

	"mexporter/internal/layout"
	"mexporter/internal/mailapi"
)

func nodes() []*layout.Node {
	return []*layout.Node{
		{Folder: mailapi.Folder{ID: "0", Name: "Входящие", MessagesTotal: 10}, Path: "Входящие"},
		{Folder: mailapi.Folder{ID: "2", Name: "booking", MessagesTotal: 0}, Path: "Входящие/booking"},
		{Folder: mailapi.Folder{ID: "23", Name: "Рассылки", MessagesTotal: 5}, Path: "Входящие/Рассылки (23)"},
	}
}

func TestModelSelection(t *testing.T) {
	m := New(nodes(), []string{"2"}, 2, 60)
	if got := m.SelectedIDs(); len(got) != 1 || got[0] != "2" {
		t.Fatalf("preselected = %v", got)
	}
	if got := m.SelectedMessages(); got != 0 {
		t.Fatalf("messages = %d", got)
	}
	if a := m.Handle(KeyDown); a != actionNone {
		t.Fatalf("down action = %v", a)
	}
	if a := m.Handle(KeyDown); a != actionNone {
		t.Fatalf("second down action = %v", a)
	}
	if a := m.Handle(KeySpace); a != actionNone {
		t.Fatalf("space action = %v", a)
	}
	if got := m.SelectedIDs(); len(got) != 2 || got[0] != "2" || got[1] != "23" {
		t.Fatalf("selected = %v", got)
	}
	if got := m.SelectedMessages(); got != 5 {
		t.Fatalf("messages = %d", got)
	}
	if a := m.Handle(KeyEnter); a != actionStart {
		t.Fatalf("enter action = %v", a)
	}
}

func TestModelAllNoneAndCancel(t *testing.T) {
	m := New(nodes(), nil, 10, 80)
	m.Handle(KeyAll)
	if got := len(m.SelectedIDs()); got != 3 {
		t.Fatalf("all = %d", got)
	}
	m.Handle(KeyNone)
	if got := len(m.SelectedIDs()); got != 0 {
		t.Fatalf("none = %d", got)
	}
	if a := m.Handle(KeyEnter); a != actionNone || m.notice == "" {
		t.Fatalf("empty enter: action=%v notice=%q", a, m.notice)
	}
	if a := m.Handle(KeyQuit); a != actionCancel {
		t.Fatalf("quit action = %v", a)
	}
	if a := m.Handle(KeyCancel); a != actionCancel {
		t.Fatalf("cancel action = %v", a)
	}
}

func TestModelNavigation(t *testing.T) {
	m := New(nodes(), nil, 1, 60)
	m.Handle(KeyEnd)
	if m.cursor != 2 {
		t.Fatalf("end cursor = %d", m.cursor)
	}
	m.Handle(KeyHome)
	if m.cursor != 0 {
		t.Fatalf("home cursor = %d", m.cursor)
	}
	m.Handle(KeyPageDown)
	if m.cursor != 2 {
		t.Fatalf("page down cursor = %d", m.cursor)
	}
	m.Handle(KeyPageUp)
	if m.cursor != 0 {
		t.Fatalf("page up cursor = %d", m.cursor)
	}
	m.Handle(KeyUp)
	if m.cursor != 0 {
		t.Fatalf("up clamped = %d", m.cursor)
	}
	if m.offset < 0 || m.offset > 2 {
		t.Fatalf("offset = %d", m.offset)
	}
}

func TestViewAndFit(t *testing.T) {
	m := New(nodes(), []string{"0"}, 5, 40)
	view := m.View("user@mail_ru_hosted_domain")
	for _, want := range []string{"mexporter", "user@mail_ru_hosted_domain", "[x] Входящие", "[ ] Входящие/booking", "Enter"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if got := fit("abcdef", 3); got != "abc" {
		t.Fatalf("fit truncate = %q", got)
	}
	if got := fit("ab", 4); got != "ab  " {
		t.Fatalf("fit pad = %q", got)
	}
	if got := fit("abc", 0); got != "abc" {
		t.Fatalf("fit zero = %q", got)
	}
}

func TestApplyLine(t *testing.T) {
	m := New(nodes(), nil, 10, 80)
	if err := applyLine(m, "1, 3"); err != nil {
		t.Fatal(err)
	}
	if got := m.SelectedIDs(); len(got) != 2 || got[0] != "0" || got[1] != "23" {
		t.Fatalf("selected = %v", got)
	}
	if err := applyLine(m, "all"); err != nil {
		t.Fatal(err)
	}
	if got := len(m.SelectedIDs()); got != 3 {
		t.Fatalf("all = %d", got)
	}
	if err := applyLine(m, "none"); err != nil {
		t.Fatal(err)
	}
	if got := len(m.SelectedIDs()); got != 0 {
		t.Fatalf("none = %d", got)
	}
	for _, bad := range []string{"0", "4", "x", "-1"} {
		if err := applyLine(m, bad); err == nil {
			t.Fatalf("applyLine(%q) accepted", bad)
		}
	}
}

func TestEmptyModel(t *testing.T) {
	m := New(nil, nil, 0, 0)
	if m.height != 10 || m.width != 100 {
		t.Fatalf("defaults = %d x %d", m.height, m.width)
	}
	m.Handle(KeyEnd)
	m.Handle(KeySpace)
	if got := m.SelectedIDs(); len(got) != 0 {
		t.Fatalf("selected = %v", got)
	}
	if view := m.View("x"); !strings.Contains(view, "Папок: 0") {
		t.Fatalf("view = %q", view)
	}
}
