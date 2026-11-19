package tui

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type fakeTerm struct {
	keys   []key
	closed bool
}

func (f *fakeTerm) ReadKey() (key, error) {
	if len(f.keys) == 0 {
		return keyUnknown, io.EOF
	}
	k := f.keys[0]
	f.keys = f.keys[1:]
	return k, nil
}

func (f *fakeTerm) Size() (int, int) { return 20, 80 }

func (f *fakeTerm) Close() error {
	f.closed = true
	return errors.New("close warning")
}

func TestRunInteractive(t *testing.T) {
	term := &fakeTerm{keys: []key{keyDown, keyDown, keySpace, keyEnter}}
	ids, err := run(nodes(), nil, "acct", func() (terminal, error) { return term, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "23" {
		t.Fatalf("ids = %v", ids)
	}
	if !term.closed {
		t.Fatal("terminal was not closed")
	}
}

func TestRunCancelAndReadError(t *testing.T) {
	_, err := run(nodes(), nil, "acct", func() (terminal, error) {
		return &fakeTerm{keys: []key{keyQuit}}, nil
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("cancel err = %v", err)
	}
	_, err = run(nodes(), nil, "acct", func() (terminal, error) {
		return &fakeTerm{}, nil
	})
	if !errors.Is(err, io.EOF) {
		t.Fatalf("read err = %v", err)
	}
}

func TestRunLine(t *testing.T) {
	ids, err := runLine(nodes(), nil, strings.NewReader("1\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "0" {
		t.Fatalf("ids = %v", ids)
	}
	ids, err = runLine(nodes(), nil, strings.NewReader("all\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 {
		t.Fatalf("all ids = %v", ids)
	}
}

func TestRunLineCancelAndBadInput(t *testing.T) {
	for _, input := range []string{"q\n", "й\n", ""} {
		if _, err := runLine(nodes(), nil, strings.NewReader(input)); !errors.Is(err, ErrCanceled) {
			t.Fatalf("input %q: err = %v", input, err)
		}
	}
	if _, err := runLine(nodes(), nil, strings.NewReader("99\n0\n\n")); !errors.Is(err, ErrCanceled) {
		t.Fatalf("bad input err = %v", err)
	}
}
