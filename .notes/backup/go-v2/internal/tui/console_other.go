//go:build !windows

package tui

import "errors"

// openTerminal is unsupported outside Windows; Run falls back to the line UI.
func openTerminal() (terminal, error) {
	return nil, errors.New("raw terminal is supported on Windows only")
}
