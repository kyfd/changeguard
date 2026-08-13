package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestExportDoesNotModifyStateFile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	outputPath := filepath.Join(dir, "bundle.json")
	content := []byte(`{"organizations":[{"id":"org","name":"Org"}],"changes":[{"organization_id":"org","id":"chg","title":"Test","artifact_sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","version":1}],"audits":[]}`)
	if err := os.WriteFile(statePath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(statePath, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(statePath)
	if err := run([]string{"export", "-data", statePath, "-change", "chg", "-out", outputPath}); err != nil {
		t.Fatal(err)
	}
	afterContent, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(statePath)
	if !bytes.Equal(content, afterContent) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("export modified source state bytes or mtime")
	}
	if err := run([]string{"verify", "-in", outputPath}); err != nil {
		t.Fatal(err)
	}
}
