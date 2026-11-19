//go:build windows

package tui

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadKeySequences(t *testing.T) {
	tests := []struct {
		input string
		want  Key
	}{
		{"\x03", KeyCancel},
		{"\r", KeyEnter},
		{"\n", KeyEnter},
		{" ", KeySpace},
		{"a", KeyAll},
		{"A", KeyAll},
		{"n", KeyNone},
		{"N", KeyNone},
		{"q", KeyQuit},
		{"Q", KeyQuit},
		{"j", KeyDown},
		{"J", KeyDown},
		{"k", KeyUp},
		{"K", KeyUp},
		{"?", KeyUnknown},
		{"ф", KeyAll},
		{"Ф", KeyAll},
		{"т", KeyNone},
		{"Т", KeyNone},
		{"й", KeyQuit},
		{"Й", KeyQuit},
		{"о", KeyDown},
		{"О", KeyDown},
		{"л", KeyUp},
		{"Л", KeyUp},
		{"Ы", KeyUnknown},
		{"\x1b[A", KeyUp},
		{"\x1b[B", KeyDown},
		{"\x1b[H", KeyHome},
		{"\x1b[F", KeyEnd},
		{"\x1bOA", KeyUp},
		{"\x1b[5~", KeyPageUp},
		{"\x1b[6~", KeyPageDown},
		{"\x1b[5", KeyUnknown},
		{"\x1b[C", KeyUnknown},
		{"\x1bZ", KeyUnknown},
		{"\x1b", KeyQuit},
	}
	for _, tt := range tests {
		term := &winTerminal{reader: bufio.NewReader(strings.NewReader(tt.input))}
		got, err := term.ReadKey()
		if err != nil {
			t.Fatalf("%q: err = %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("%q: key = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestReadKeyIncompleteRune(t *testing.T) {
	term := &winTerminal{reader: bufio.NewReader(strings.NewReader("\xd0"))}
	if _, err := term.ReadKey(); err == nil {
		t.Fatal("expected incomplete rune error")
	}
}

func TestReadKeyEOF(t *testing.T) {
	term := &winTerminal{reader: bufio.NewReader(strings.NewReader(""))}
	if _, err := term.ReadKey(); err == nil {
		t.Fatal("expected EOF")
	}
}
