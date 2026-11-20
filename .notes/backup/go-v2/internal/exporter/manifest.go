package exporter

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"mexporter/internal/layout"
)

// manifestRecord is one row of folders.json.
type manifestRecord struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Parent   string `json:"parent"`
	Path     string `json:"path"`
	Messages int    `json:"messages"`
}

// WriteManifest records the resolved folder tree next to the journal.
//
// REQ-EXPORT-005: paths in the manifest match the on-disk directory names.
func WriteManifest(accountDir string, nodes []*layout.Node) error {
	recs := make([]manifestRecord, 0, len(nodes))
	for _, n := range nodes {
		recs = append(recs, manifestRecord{
			ID:       n.ID.String(),
			Name:     n.Name,
			Parent:   n.Parent.String(),
			Path:     n.Path,
			Messages: n.MessagesTotal,
		})
	}
	raw, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return fmt.Errorf("REQ-EXPORT-005: encode manifest: %w", err)
	}
	raw = append(raw, '\n')
	dest := filepath.Join(accountDir, "folders.json")
	if err := writeFileAtomic(dest, raw); err != nil {
		return fmt.Errorf("REQ-EXPORT-005: write manifest: %w", err)
	}
	return nil
}
