package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func writeTestManifest(t *testing.T, directory string, input manifest) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, ".changeguard.json")
	if err := os.WriteFile(filename, payload, 0600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func TestDigestFromManifestRejectsTraversal(t *testing.T) {
	manifestDir := t.TempDir()
	externalDir := t.TempDir()
	externalFile := filepath.Join(externalDir, "secret.yaml")
	if err := os.WriteFile(externalFile, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(manifestDir, externalFile)
	if err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, manifestDir, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: filepath.ToSlash(relative)}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("path traversal must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRejectsAbsolutePath(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(artifact, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: artifact}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "relative") {
		t.Fatalf("absolute artifact path must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRejectsOversizedArtifact(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "large.yaml")
	if err := os.WriteFile(artifact, bytes.Repeat([]byte("x"), int(maxArtifactFileBytes)+1), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Path: "large.yaml"}}})
	if _, err := digestFromManifest(filename); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized artifact must be rejected, got %v", err)
	}
}

func TestDigestFromManifestRehashesActualFile(t *testing.T) {
	directory := t.TempDir()
	artifact := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(artifact, []byte("debug: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	filename := writeTestManifest(t, directory, manifest{Version: 1, Environment: "生产环境", ChangeType: "配置变更", Artifacts: []manifestArtifact{{Kind: model.ArtifactConfig, Name: "config", Path: "config.yaml"}}})
	first, err := digestFromManifest(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("debug: false"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := digestFromManifest(filename)
	if err != nil {
		t.Fatal(err)
	}
	if first.ArtifactSHA256 == second.ArtifactSHA256 {
		t.Fatal("CI digest must bind exact current repository bytes")
	}
}
