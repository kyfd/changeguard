package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCoreReleaseScriptsCarryMachineVerifiableProvenance(t *testing.T) {
	production := filepath.Join("..", "..", "deploy", "production")
	assertMarkers(t, filepath.Join(production, "source-tree-sha256.sh"), []string{
		"find cmd internal -type f -print0", "go.mod\\0go.sum\\0", "LC_ALL=C sort -z",
	})
	assertMarkers(t, filepath.Join(production, "build-core-release.sh"), []string{
		"CHANGEGUARD_VERSION", "CHANGEGUARD_COMMIT", "CHANGEGUARD_SOURCE_SHA256",
		"internal/buildinfo.Version", "internal/buildinfo.SourceSHA256", "artifact_sha256=",
	})
	assertMarkers(t, filepath.Join(production, "build-core-git-release.sh"), []string{
		"status --porcelain=v1 --untracked-files=all",
		"release tag must be annotated",
		"changeguard-core-verification/v1",
		"verification evidence {field} mismatch",
		`"source_sha256": sys.argv[5]`,
		"CHANGEGUARD_GOMODCACHE",
		"-u http_proxy -u https_proxy",
		"GOPROXY=off",
		"release directory retained for audit with .incomplete marker",
		"bundle create",
		"bundle verify",
		"archive --format=tar.gz",
		"changeguard-core-release/v2",
		"SHA256SUMS",
	})
}

func TestCoreGovernanceAlertRulesCoverOperationalOutcomes(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "production", "dbguard-prometheus-alerts.yaml")
	assertMarkers(t, path, []string{
		"ChangeGuardCoreBuildProvenanceUnverified",
		"ChangeGuardLinkedIncidentOpen",
		"ChangeGuardRollbackFailureObserved",
		"ChangeGuardPostReleaseRemediationRateHigh",
		"ChangeGuardBusinessSLIDegraded",
		"ChangeGuardBusinessObjectiveAttainmentLow",
		"ChangeGuardBusinessOutcomeEvidenceMissing",
		"dbguard_governance_post_release_samples >= 5",
		"and on() dbguard_governance_outcome_signal_observable",
	})
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"change_id=", "incident_id=", "metric_name="} {
		if strings.Contains(string(content), forbidden) {
			t.Fatalf("alert rules must not introduce high-cardinality labels: %q", forbidden)
		}
	}
}

func assertMarkers(t *testing.T, path string, markers []string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range markers {
		if !strings.Contains(string(content), marker) {
			t.Fatalf("%s is missing %q", path, marker)
		}
	}
}
