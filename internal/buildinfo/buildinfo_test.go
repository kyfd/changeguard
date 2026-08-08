package buildinfo

import (
	"strings"
	"testing"
)

func TestCurrentVerifiesCompleteReleaseProvenance(t *testing.T) {
	restore := replaceBuildValues("1.4.0", "abcdef1234567890", "2026-08-08T08:00:00Z", strings.Repeat("a", 64))
	defer restore()

	info := Current()
	if !info.ProvenanceVerified {
		t.Fatalf("complete provenance must verify: %+v", info)
	}
	if info.GoVersion == "" || !strings.Contains(info.String(), "provenance_verified=true") {
		t.Fatalf("build info is incomplete: %+v", info)
	}
}

func TestCurrentRejectsDevelopmentOrMalformedProvenance(t *testing.T) {
	tests := []struct {
		version, commit, builtAt, source string
	}{
		{"dev", "abcdef1", "2026-08-08T08:00:00Z", strings.Repeat("a", 64)},
		{"1.4.0", "not-a-commit", "2026-08-08T08:00:00Z", strings.Repeat("a", 64)},
		{"1.4.0", "abcdef1", "not-a-time", strings.Repeat("a", 64)},
		{"1.4.0", "abcdef1", "2026-08-08T08:00:00Z", "short"},
	}
	for _, item := range tests {
		restore := replaceBuildValues(item.version, item.commit, item.builtAt, item.source)
		if info := Current(); info.ProvenanceVerified {
			restore()
			t.Fatalf("malformed provenance unexpectedly verified: %+v", info)
		}
		restore()
	}
}

func replaceBuildValues(version, commit, builtAt, source string) func() {
	oldVersion, oldCommit, oldBuiltAt, oldSource := Version, Commit, BuiltAt, SourceSHA256
	Version, Commit, BuiltAt, SourceSHA256 = version, commit, builtAt, source
	return func() {
		Version, Commit, BuiltAt, SourceSHA256 = oldVersion, oldCommit, oldBuiltAt, oldSource
	}
}
