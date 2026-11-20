package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// FolderNode is a folder with its resolved, collision-free path.
type FolderNode struct {
	Folder
	Path     string // relative path under <out>/<email>/, e.g. "Входящие/Рассылки (23)"
	Children []*FolderNode
}

var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// SanitizeName makes a string safe as a Windows file or directory name.
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

// BuildTree resolves parents and assigns unique paths to all folders.
func BuildTree(folders []Folder) ([]*FolderNode, map[string]*FolderNode) {
	byID := make(map[string]*FolderNode, len(folders))
	for _, f := range folders {
		id := string(f.ID)
		if id == "" {
			continue
		}
		byID[id] = &FolderNode{Folder: f}
	}

	var roots []*FolderNode
	for _, f := range folders {
		node, ok := byID[string(f.ID)]
		if !ok {
			continue
		}
		parentID := string(f.Parent)
		parent, hasParent := byID[parentID]
		if hasParent && parent != node && parentID != "" && parentID != "-1" {
			parent.Children = append(parent.Children, node)
		} else {
			roots = append(roots, node)
		}
	}

	var assign func(siblings []*FolderNode, prefix string)
	assign = func(siblings []*FolderNode, prefix string) {
		counts := map[string]int{}
		for _, n := range siblings {
			base := SanitizeName(n.Name, 80)
			counts[base]++
		}
		used := map[string]bool{}
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
			assign(n.Children, n.Path)
		}
	}
	assign(roots, "")

	sort.SliceStable(roots, func(i, j int) bool {
		return roots[i].Path < roots[j].Path
	})
	return roots, byID
}

// Flatten returns nodes in a stable parent-before-child order.
func Flatten(roots []*FolderNode) []*FolderNode {
	var out []*FolderNode
	var walk func(n *FolderNode)
	walk = func(n *FolderNode) {
		out = append(out, n)
		kids := append([]*FolderNode(nil), n.Children...)
		sort.SliceStable(kids, func(i, j int) bool { return kids[i].Path < kids[j].Path })
		for _, c := range kids {
			walk(c)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	return out
}

// SafeFilename builds a readable, unique .eml filename.
// Format: YYYY-MM-DD_<subject>_<uidl>.eml
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
	r := []rune(name)
	if len(r) > 120 {
		name = string(r[:120])
	}
	return name + ".eml"
}
