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
		"go mod download all",
		"go mod verify",
		"module-verify.txt",
		"release directory retained for audit with .incomplete marker",
		"bundle create",
		"bundle verify",
		"archive --format=tar.gz",
		"changeguard-core-release/v2",
		"SHA256SUMS",
	})
}

func TestProductionCoreServiceFailsClosedBeforeRunning(t *testing.T) {
	production := filepath.Join("..", "..", "deploy", "production")
	assertMarkers(t, filepath.Join(production, "changeguard.service"), []string{
		"User=changeguard",
		"Group=changeguard",
		"Environment=DBGUARD_ENV_FILE=/etc/changeguard/core.env",
		"Environment=DBGUARD_ENV_PROFILE=production",
		"EnvironmentFile=/etc/changeguard/core.env",
		"changeguard-core-preflight.sh",
		"dbguard --check-config",
		"ProtectSystem=strict",
		"ReadWritePaths=/opt/changeguard/data",
		"CapabilityBoundingSet=",
		"UMask=0077",
	})
	assertMarkers(t, filepath.Join(production, "changeguard-core-preflight.sh"), []string{
		"runtime_user_must_not_be_root",
		"canonical_env_writable_by_runtime_user",
		"release_outside_release_root",
		"sha256sum -c SHA256SUMS",
		"release_sha256_mismatch",
		"migration_witness_pair_incomplete",
		"changeguard-migration-witness-required/v1",
		"core_preflight=passed",
	})
	assertMarkers(t, filepath.Join(production, "changeguard-core.env.example"), []string{
		"DBGUARD_SESSION_MODE=redis",
		"DBGUARD_EXPERIMENT_MODE=postgres",
		"DBGUARD_MIGRATION_WITNESS_FILE=/opt/changeguard/data/dbguard.json.rollback-witness.json",
		"DBGUARD_METRICS_TOKEN=REPLACE_ME",
		"DBGUARD_OPERATIONS_WEBHOOK_TOKEN=REPLACE_ME",
	})
	assertMarkers(t, filepath.Join(production, "changeguard-backup.sh"), []string{
		"CHANGEGUARD_CORE_ENV_FILE",
		"CHANGEGUARD_CORE_DATA_FILE",
		`"scope": "full"`,
		`"migration_witness_present"`,
		"manifest.sha256",
	})
	assertMarkers(t, filepath.Join(production, "changeguard-restore.sh"), []string{
		"changeguard-restore-verification/v1",
		"snapshot_outside_backup_root",
		"manifest coverage mismatch",
		"migration witness payload digest mismatch",
		"restore_destination_outside_restore_root",
		"restore-manifest.sha256",
		"restore_status=",
	})
	assertMarkers(t, filepath.Join(production, "changeguard-core-install.sh"), []string{
		"installer_must_run_as_root",
		"archive_sha256_mismatch",
		"archive member contains path traversal",
		"archive contains links or special files",
		"release checksum coverage mismatch",
		"installed_release_is_group_or_world_writable",
		"core_install=passed",
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
