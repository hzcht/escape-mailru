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
		want  key
	}{
		{"\x03", keyCancel},
		{"\r", keyEnter},
		{"\n", keyEnter},
		{" ", keySpace},
		{"a", keyAll},
		{"A", keyAll},
		{"n", keyNone},
		{"N", keyNone},
		{"q", keyQuit},
		{"Q", keyQuit},
		{"j", keyDown},
		{"J", keyDown},
		{"k", keyUp},
		{"K", keyUp},
		{"?", keyUnknown},
		{"ф", keyAll},
		{"Ф", keyAll},
		{"т", keyNone},
		{"Т", keyNone},
		{"й", keyQuit},
		{"Й", keyQuit},
		{"о", keyDown},
		{"О", keyDown},
		{"л", keyUp},
		{"Л", keyUp},
		{"Ы", keyUnknown},
		{"\x1b[A", keyUp},
		{"\x1b[B", keyDown},
		{"\x1b[H", keyHome},
		{"\x1b[F", keyEnd},
		{"\x1bOA", keyUp},
		{"\x1b[5~", keyPageUp},
		{"\x1b[6~", keyPageDown},
		{"\x1b[5", keyUnknown},
		{"\x1b[C", keyUnknown},
		{"\x1bZ", keyUnknown},
		{"\x1b", keyQuit},
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
