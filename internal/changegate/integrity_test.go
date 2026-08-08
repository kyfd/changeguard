package changegate

import (
	"strings"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestPrepareArtifactHashesOriginalBytesBeforeRedaction(t *testing.T) {
	raw := "api_key: actual-secret\n"
	artifact := PrepareArtifact(model.ChangeArtifact{Kind: model.ArtifactConfig, Name: "app.yaml", Content: raw})
	if artifact.ContentSHA256 != SHA256(raw) {
		t.Fatalf("content hash must bind exact original bytes: got %s want %s", artifact.ContentSHA256, SHA256(raw))
	}
	if strings.Contains(artifact.Content, "actual-secret") || !strings.Contains(artifact.Content, "[REDACTED]") {
		t.Fatalf("persisted content must be redacted: %q", artifact.Content)
	}
}

func TestPrepareStoredArtifactPreservesOriginalHashAcrossRedaction(t *testing.T) {
	raw := "api_key: actual-secret\n"
	originalSHA256 := SHA256(raw)
	first := PrepareStoredArtifact(model.ChangeArtifact{
		Kind: model.ArtifactConfig, Name: "app.yaml", Content: raw, ContentSHA256: originalSHA256,
	})
	second := PrepareStoredArtifact(first)
	if first.ContentSHA256 != originalSHA256 || second.ContentSHA256 != originalSHA256 {
		t.Fatalf("stored artifact lost original byte digest: first=%s second=%s", first.ContentSHA256, second.ContentSHA256)
	}
	if first.Content != second.Content || strings.Contains(second.Content, "actual-secret") {
		t.Fatalf("stored artifact redaction is not idempotent: first=%q second=%q", first.Content, second.Content)
	}
}

func TestKubernetesRedactsSecretButPreservesConfigMapData(t *testing.T) {
	raw := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: visible\ndata:\n  password: config-map-value\n---\napiVersion: v1\nkind: Secret\nmetadata:\n  name: hidden\nstringData:\n  password: secret-value\n  username: demo"
	artifact := PrepareArtifact(model.ChangeArtifact{Kind: model.ArtifactKubernetes, Content: raw})
	if !strings.Contains(artifact.Content, "password: config-map-value") {
		t.Fatalf("ConfigMap data must remain reviewable: %s", artifact.Content)
	}
	if strings.Contains(artifact.Content, "secret-value") || strings.Contains(artifact.Content, "username: demo") {
		t.Fatalf("Secret data/stringData values must be redacted: %s", artifact.Content)
	}
	if strings.Count(artifact.Content, "[REDACTED]") < 2 {
		t.Fatalf("expected all Secret entries to be redacted: %s", artifact.Content)
	}
}

func TestChangeDigestChangesWithExactArtifactBytes(t *testing.T) {
	one := PrepareArtifact(model.ChangeArtifact{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\n"})
	two := PrepareArtifact(model.ChangeArtifact{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false"})
	digestOne := ChangeDigest("生产环境", "配置变更", []model.ChangeArtifact{one}, SHA256(""), SHA256(""), "restore")
	digestTwo := ChangeDigest("生产环境", "配置变更", []model.ChangeArtifact{two}, SHA256(""), SHA256(""), "restore")
	if digestOne == digestTwo {
		t.Fatal("digest must change when exact artifact bytes change")
	}
}
