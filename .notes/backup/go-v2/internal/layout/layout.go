// Package layout resolves folder trees and filesystem-safe names.
package layout

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"mexporter/internal/mailapi"
)

// reservedNames are Windows device names that cannot be used as files.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// Node is a folder with its resolved path below the account directory.
type Node struct {
	mailapi.Folder
	Path     string
	Depth    int
	Children []*Node
}

// SanitizeName makes a string safe as a Windows file or directory name.
//
// REQ-EXPORT-005: unsafe characters are replaced, trailing dots/spaces removed.
func SanitizeName(name string, maxRunes int) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			return '_'
		}
		if r < 32 {
			return '_'
		}
		return r
	}, name)
	name = strings.Join(strings.Fields(name), " ")
	name = strings.Trim(name, " .")
	if name == "" {
		name = "_"
	}
	if maxRunes > 0 {
		runes := []rune(name)
		if len(runes) > maxRunes {
			name = strings.TrimRight(string(runes[:maxRunes]), " .")
			if name == "" {
				name = "_"
			}
		}
	}
	upper := strings.ToUpper(name)
	if i := strings.IndexByte(upper, '.'); i > 0 {
		upper = upper[:i]
	}
	if reservedNames[upper] {
		name = "_" + name
	}
	return name
}

// BuildTree links folders to parents and assigns unique sibling paths.
//
// REQ-EXPORT-005: duplicate sibling names get an id suffix, e.g. "Рассылки (23)".
func BuildTree(folders []mailapi.Folder) ([]*Node, map[string]*Node) {
	byID := make(map[string]*Node, len(folders))
	for _, f := range folders {
		id := f.ID.String()
		if id == "" {
			continue
		}
		if _, ok := byID[id]; !ok {
			byID[id] = &Node{Folder: f}
		}
	}
	roots := linkNodes(folders, byID)
	assignAll(roots)
	return roots, byID
}

// linkNodes attaches children and collects roots. It never recurses.
func linkNodes(folders []mailapi.Folder, byID map[string]*Node) []*Node {
	var roots []*Node
	seen := make(map[string]bool, len(folders))
	for _, f := range folders {
		id := f.ID.String()
		node, ok := byID[id]
		if !ok || seen[id] {
			continue
		}
		seen[id] = true
		pid := f.Parent.String()
		parent, hasParent := byID[pid]
		if hasParent && parent != node && pid != "" && pid != "-1" {
			parent.Children = append(parent.Children, node)
			continue
		}
		roots = append(roots, node)
	}
	return roots
}

// assignAll walks the tree with an explicit stack and assigns paths.
func assignAll(roots []*Node) {
	type level struct {
		nodes  []*Node
		prefix string
	}
	stack := []level{{nodes: roots}}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		assignPaths(cur.nodes, cur.prefix)
		for _, n := range cur.nodes {
			if len(n.Children) == 0 {
				continue
			}
			sort.SliceStable(n.Children, func(i, j int) bool { return n.Children[i].Name < n.Children[j].Name })
			stack = append(stack, level{nodes: n.Children, prefix: n.Path})
		}
	}
}

// assignPaths gives every sibling a unique path segment.
func assignPaths(siblings []*Node, prefix string) {
	counts := make(map[string]int, len(siblings))
	for _, n := range siblings {
		counts[SanitizeName(n.Name, 80)]++
	}
	used := make(map[string]bool, len(siblings))
	for _, n := range siblings {
		base := SanitizeName(n.Name, 80)
		if counts[base] > 1 {
			base = fmt.Sprintf("%s (%s)", base, n.ID)
		}
		candidate := base
		for i := 2; used[strings.ToLower(candidate)]; i++ {
			candidate = fmt.Sprintf("%s (%d)", base, i)
		}
		used[strings.ToLower(candidate)] = true
		if prefix == "" {
			n.Path = candidate
		} else {
			n.Path = prefix + "/" + candidate
		}
		n.Depth = strings.Count(n.Path, "/")
	}
}

// Flatten returns nodes parent-before-child with stable sibling order.
func Flatten(roots []*Node) []*Node {
	out := make([]*Node, 0, len(roots))
	stack := make([]*Node, 0, len(roots))
	for i := len(roots) - 1; i >= 0; i-- {
		stack = append(stack, roots[i])
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		out = append(out, n)
		for i := len(n.Children) - 1; i >= 0; i-- {
			stack = append(stack, n.Children[i])
		}
	}
	return out
}

// SafeFilename builds a readable, unique .eml name.
//
// REQ-EXPORT-005: format is YYYY-MM-DD_<subject>_<uidl>.eml.
func SafeFilename(dateUnix int64, subject, uidl string) string {
	date := "no-date"
	if dateUnix > 0 {
		date = time.Unix(dateUnix, 0).Format("2006-01-02")
	}
	subj := SanitizeName(subject, 50)
	if subj == "_" {
		subj = ""
	}
	name := date
	if subj != "" {
		name += "_" + subj
	}
	name += "_" + uidl
	runes := []rune(name)
	if len(runes) > 120 {
		name = string(runes[:120])
	}
	return name + ".eml"
}
