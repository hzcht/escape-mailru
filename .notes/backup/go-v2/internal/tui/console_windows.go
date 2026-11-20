//go:build windows

package tui

import (
	"bufio"
	"fmt"
	"os"
	"syscall"
	"unicode/utf8"
	"unsafe"
)

const (
	enableProcessedInput         = 0x0001
	enableLineInput              = 0x0002
	enableEchoInput              = 0x0004
	enableVirtualTerminalInput   = 0x0200
	enableVirtualTerminalProcess = 0x0004
	defaultTerminalWidth         = 100
	defaultTerminalHeight        = 30
)

var (
	kernel32                       = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode             = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode             = kernel32.NewProc("SetConsoleMode")
	procGetConsoleScreenBufferInfo = kernel32.NewProc("GetConsoleScreenBufferInfo")
)

type coord struct {
	X int16
	Y int16
}

type smallRect struct {
	Left   int16
	Top    int16
	Right  int16
	Bottom int16
}

type screenBufferInfo struct {
	Size              coord
	CursorPosition    coord
	Attributes        uint16
	Window            smallRect
	MaximumWindowSize coord
}

type winTerminal struct {
	in      syscall.Handle
	out     syscall.Handle
	inMode  uint32
	outMode uint32
	reader  *bufio.Reader
}

// openTerminal switches the Windows console to raw VT input.
func openTerminal() (terminal, error) {
	in := syscall.Handle(os.Stdin.Fd())
	out := syscall.Handle(os.Stdout.Fd())
	inMode, err := consoleMode(in)
	if err != nil {
		return nil, err
	}
	outMode, err := consoleMode(out)
	if err != nil {
		return nil, err
	}
	raw := inMode &^ uint32(enableLineInput|enableEchoInput|enableProcessedInput)
	raw |= enableVirtualTerminalInput
	if err := setConsoleMode(in, raw); err != nil {
		return nil, err
	}
	if err := setConsoleMode(out, outMode|enableVirtualTerminalProcess); err != nil {
		setConsoleMode(in, inMode) // NOERR: best-effort rollback.
		return nil, err
	}
	return &winTerminal{in: in, out: out, inMode: inMode, outMode: outMode, reader: bufio.NewReader(os.Stdin)}, nil
}

// consoleMode reads a console mode.
func consoleMode(h syscall.Handle) (uint32, error) {
	var mode uint32
	ok, _, callErr := procGetConsoleMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if ok == 0 {
		return 0, fmt.Errorf("GetConsoleMode: %w", callErr)
	}
	return mode, nil
}

// setConsoleMode writes a console mode.
func setConsoleMode(h syscall.Handle, mode uint32) error {
	ok, _, callErr := procSetConsoleMode.Call(uintptr(h), uintptr(mode))
	if ok == 0 {
		return fmt.Errorf("SetConsoleMode: %w", callErr)
	}
	return nil
}

// ReadKey blocks for one normalized key.
func (t *winTerminal) ReadKey() (Key, error) {
	b, err := t.reader.ReadByte()
	if err != nil {
		return KeyUnknown, err
	}
	if b >= utf8.RuneSelf {
		return t.readCyrillic(b)
	}
	switch b {
	case 3:
		return KeyCancel, nil
	case 13, 10:
		return KeyEnter, nil
	case ' ':
		return KeySpace, nil
	case 'a', 'A':
		return KeyAll, nil
	case 'n', 'N':
		return KeyNone, nil
	case 'q', 'Q':
		return KeyQuit, nil
	case 'j', 'J':
		return KeyDown, nil
	case 'k', 'K':
		return KeyUp, nil
	case 27:
		return t.readEscape()
	}
	return KeyUnknown, nil
}

// readCyrillic maps Russian-layout shortcuts to the same actions.
func (t *winTerminal) readCyrillic(first byte) (Key, error) {
	buf := []byte{first}
	for len(buf) < utf8.UTFMax {
		if utf8.FullRune(buf) {
			break
		}
		b, err := t.reader.ReadByte()
		if err != nil {
			return KeyUnknown, err
		}
		buf = append(buf, b)
	}
	switch r, _ := utf8.DecodeRune(buf); r {
	case 'ф', 'Ф':
		return KeyAll, nil
	case 'т', 'Т':
		return KeyNone, nil
	case 'й', 'Й':
		return KeyQuit, nil
	case 'о', 'О':
		return KeyDown, nil
	case 'л', 'Л':
		return KeyUp, nil
	}
	return KeyUnknown, nil
}

// readEscape parses a VT escape sequence. Bare Escape is not advertised.
func (t *winTerminal) readEscape() (Key, error) {
	b, err := t.reader.ReadByte()
	if err != nil {
		return KeyQuit, nil
	}
	if b != '[' && b != 'O' {
		return KeyUnknown, nil
	}
	c, err := t.reader.ReadByte()
	if err != nil {
		return KeyQuit, nil
	}
	switch c {
	case 'A':
		return KeyUp, nil
	case 'B':
		return KeyDown, nil
	case 'H':
		return KeyHome, nil
	case 'F':
		return KeyEnd, nil
	case '5', '6':
		if tail, err := t.reader.ReadByte(); err == nil && tail == '~' {
			if c == '5' {
				return KeyPageUp, nil
			}
			return KeyPageDown, nil
		}
	}
	return KeyUnknown, nil
}

// Size returns the console window size.
func (t *winTerminal) Size() (int, int) {
	var info screenBufferInfo
	ok, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(t.out), uintptr(unsafe.Pointer(&info)))
	if ok == 0 {
		return defaultTerminalHeight, defaultTerminalWidth
	}
	width := int(info.Window.Right-info.Window.Left) + 1
	height := int(info.Window.Bottom-info.Window.Top) + 1
	if width <= 0 || height <= 0 {
		return defaultTerminalHeight, defaultTerminalWidth
	}
	return height, width
}

// Close restores the original console modes.
func (t *winTerminal) Close() error {
	errOut := setConsoleMode(t.out, t.outMode)
	errIn := setConsoleMode(t.in, t.inMode)
	if errIn != nil {
		return errIn
	}
	return errOut
}
