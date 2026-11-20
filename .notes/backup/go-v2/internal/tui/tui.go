// Package tui lets the operator choose folders before an export.
//
// REQ-EXPORT-006: the selection is returned to the caller, which persists it
// in the config before starting the run.
package tui

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode"

	"mexporter/internal/layout"
)

// ErrCanceled means the operator quit without starting an export.
var ErrCanceled = errors.New("selection canceled")

// terminal abstracts the raw console used by Run.
type terminal interface {
	ReadKey() (Key, error)
	Size() (int, int)
	Close() error
}

// Key is a normalized terminal key.
type Key int

// Supported keys.
const (
	KeyUnknown Key = iota
	KeyUp
	KeyDown
	KeyPageUp
	KeyPageDown
	KeyHome
	KeyEnd
	KeySpace
	KeyAll
	KeyNone
	KeyEnter
	KeyQuit
	KeyCancel
)

type action int

const (
	actionNone action = iota
	actionStart
	actionCancel
)

// Model is the selection state; it has no terminal dependency and is testable.
type Model struct {
	items    []*layout.Node
	selected map[string]bool
	cursor   int
	offset   int
	height   int
	width    int
	notice   string
}

// New builds a selection model with the given viewport.
func New(nodes []*layout.Node, preselected []string, height, width int) *Model {
	if height < 5 {
		height = 10
	}
	if width < 40 {
		width = 100
	}
	selected := make(map[string]bool, len(preselected))
	for _, id := range preselected {
		selected[id] = true
	}
	return &Model{items: nodes, selected: selected, height: height, width: width}
}

// Handle applies a key and reports whether the dialog should start or cancel.
func (m *Model) Handle(k Key) action {
	switch k {
	case KeyUp:
		m.move(-1)
	case KeyDown:
		m.move(1)
	case KeyPageUp:
		m.move(-m.height)
	case KeyPageDown:
		m.move(m.height)
	case KeyHome:
		m.cursor = 0
	case KeyEnd:
		m.cursor = len(m.items) - 1
	case KeySpace:
		m.toggle()
	case KeyAll:
		m.setAll(true)
	case KeyNone:
		m.setAll(false)
	case KeyEnter:
		if len(m.SelectedIDs()) == 0 {
			m.notice = "Выберите хотя бы одну папку"
			return actionNone
		}
		return actionStart
	case KeyQuit, KeyCancel:
		return actionCancel
	}
	m.notice = ""
	m.clamp()
	return actionNone
}

// SelectedIDs returns selected folder ids in tree order.
func (m *Model) SelectedIDs() []string {
	out := make([]string, 0, len(m.selected))
	for _, n := range m.items {
		if m.selected[n.ID.String()] {
			out = append(out, n.ID.String())
		}
	}
	return out
}

// SelectedMessages sums message counts of selected folders.
func (m *Model) SelectedMessages() int {
	total := 0
	for _, n := range m.items {
		if m.selected[n.ID.String()] {
			total += n.MessagesTotal
		}
	}
	return total
}

// View renders the full screen without a trailing clear.
func (m *Model) View(account string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mexporter — выбор папок\n")
	fmt.Fprintf(&b, "Аккаунт: %s   Папок: %d   Выбрано: %d (%d писем)\n\n",
		account, len(m.items), len(m.SelectedIDs()), m.SelectedMessages())
	end := min(m.offset+m.height, len(m.items))
	for i := m.offset; i < end; i++ {
		b.WriteString(m.row(i))
		b.WriteByte('\n')
	}
	for i := end - m.offset; i < m.height; i++ {
		b.WriteByte('\n')
	}
	if m.notice != "" {
		fmt.Fprintf(&b, "! %s\n", m.notice)
	} else {
		b.WriteByte('\n')
	}
	b.WriteString("↑/↓ — движение   Space — выбрать   A — все   N — снять   Enter — начать   Q — выход\n")
	return b.String()
}

// row renders one folder line.
func (m *Model) row(i int) string {
	n := m.items[i]
	mark := "[ ]"
	if m.selected[n.ID.String()] {
		mark = "[x]"
	}
	label := fmt.Sprintf("%s %s  %d", mark, n.Path, n.MessagesTotal)
	if i == m.cursor {
		return "\x1b[7m" + fit(label, m.width) + "\x1b[0m"
	}
	return fit(label, m.width)
}

// move advances the cursor and keeps it visible.
func (m *Model) move(delta int) {
	m.cursor += delta
	m.clamp()
}

// clamp bounds the cursor and scroll offset.
func (m *Model) clamp() {
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor >= len(m.items) {
		m.cursor = len(m.items) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+m.height {
		m.offset = m.cursor - m.height + 1
	}
}

// toggle flips the folder under the cursor.
func (m *Model) toggle() {
	if len(m.items) == 0 {
		return
	}
	id := m.items[m.cursor].ID.String()
	m.selected[id] = !m.selected[id]
}

// setAll selects or clears every folder.
func (m *Model) setAll(value bool) {
	for _, n := range m.items {
		m.selected[n.ID.String()] = value
	}
}

// fit pads or truncates a line to the terminal width.
func fit(s string, width int) string {
	if width <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) > width {
		return string(r[:width])
	}
	return s + strings.Repeat(" ", width-len(r))
}

// Run shows the interactive selector. A non-console stdin falls back to a
// numbered prompt, which keeps the program usable from scripts.
func Run(nodes []*layout.Node, preselected []string, account string) ([]string, error) {
	return run(nodes, preselected, account, openTerminal)
}

// run is Run with an injectable terminal opener for tests.
func run(nodes []*layout.Node, preselected []string, account string, open func() (terminal, error)) ([]string, error) {
	term, err := open()
	if err != nil {
		return runLine(nodes, preselected, os.Stdin)
	}
	defer func() {
		if cerr := term.Close(); cerr != nil {
			fmt.Fprintf(os.Stderr, "terminal: %v\n", cerr)
		}
	}()
	height, width := term.Size()
	m := New(nodes, preselected, height-6, width)
	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h\n")
	for {
		fmt.Print("\x1b[H\x1b[2J", m.View(account))
		k, err := term.ReadKey()
		if err != nil {
			return nil, err
		}
		switch m.Handle(k) {
		case actionStart:
			return m.SelectedIDs(), nil
		case actionCancel:
			return nil, ErrCanceled
		}
	}
}

// runLine is the fallback selector for non-console stdin.
func runLine(nodes []*layout.Node, preselected []string, in io.Reader) ([]string, error) {
	m := New(nodes, preselected, 0, 0)
	printLineList(m)
	sc := bufio.NewScanner(in)
	for {
		fmt.Print("> ")
		if !sc.Scan() {
			return nil, ErrCanceled
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			if len(m.SelectedIDs()) == 0 {
				fmt.Println("Выберите хотя бы одну папку")
				continue
			}
			return m.SelectedIDs(), nil
		}
		if strings.EqualFold(line, "q") || strings.EqualFold(line, "й") {
			return nil, ErrCanceled
		}
		if err := applyLine(m, line); err != nil {
			fmt.Println(err)
			continue
		}
		printLineList(m)
	}
}

// applyLine handles numbers, comma lists and all/none commands.
func applyLine(m *Model, line string) error {
	tokens := strings.FieldsFunc(line, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})
	for _, tok := range tokens {
		switch strings.ToLower(tok) {
		case "a", "all", "все":
			m.setAll(true)
		case "n", "none", "нет":
			m.setAll(false)
		default:
			n, err := strconv.Atoi(tok)
			if err != nil || n < 1 || n > len(m.items) {
				return fmt.Errorf("нет папки %q", tok)
			}
			id := m.items[n-1].ID.String()
			m.selected[id] = !m.selected[id]
		}
	}
	return nil
}

// printLineList prints an indexed view for the fallback selector.
func printLineList(m *Model) {
	fmt.Printf("Выбрано: %d папок, %d писем\n", len(m.SelectedIDs()), m.SelectedMessages())
	for i, n := range m.items {
		mark := " "
		if m.selected[n.ID.String()] {
			mark = "x"
		}
		fmt.Printf("%3d [%s] %s (%d)\n", i+1, mark, n.Path, n.MessagesTotal)
	}
	fmt.Println("Номера через запятую/пробел; A — все, N — снять; Enter — начать; Q — выход")
}
