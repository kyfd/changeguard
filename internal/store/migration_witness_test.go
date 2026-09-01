package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
)

func TestFileMigrationWitnessRestoresLegacyStrippedIntegrity(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "false")
	t.Setenv("DBGUARD_ENABLE_DEMO_ACCOUNTS", "false")
	t.Setenv("DBGUARD_MIGRATION_WITNESS_FILE", "")
	path := filepath.Join(t.TempDir(), "dbguard.json")
	rawArtifact := "api_key: actual-production-secret\nendpoint: https://api.example.com\n"
	rawSQL := "ALTER ROLE app PASSWORD = actual-database-secret;"
	rawRollback := "ALTER ROLE app PASSWORD = previous-database-secret;"
	initial := state{
		Organizations: []model.Organization{{ID: "org_prod", Name: "Production"}},
		Changes: []model.ChangeRequest{{
			ID: "chg_secret", OrganizationID: "org_prod", Environment: "生产", ChangeType: "配置与数据库变更",
			SQL: rawSQL, RollbackSQL: rawRollback, RollbackPlan: "password: emergency-secret",
			Artifacts: []model.ChangeArtifact{{
				ID: "artifact_stable", Kind: model.ArtifactConfig, Name: "production.yaml", Content: rawArtifact,
			}},
		}},
	}
	writeStateFixture(t, path, initial)

	first, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	firstStatus := first.MigrationWitnessStatus()
	first.Close()
	if !firstStatus.Enabled || firstStatus.Reconciliation != "initialized" {
		t.Fatalf("unexpected first witness status: %+v", firstStatus)
	}
	candidateState := readFileFixture(t, path)
	witnessPath := path + ".rollback-witness.json"
	markerPath := witnessPath + ".required"
	witnessBefore := readFileFixture(t, witnessPath)
	if !bytes.Equal(readFileFixture(t, markerPath), []byte(migrationWitnessMarkerContent)) {
		t.Fatal("migration witness marker mismatch")
	}
	for _, secret := range []string{"actual-production-secret", "actual-database-secret", "previous-database-secret", "emergency-secret"} {
		if bytes.Contains(candidateState, []byte(secret)) || bytes.Contains(witnessBefore, []byte(secret)) {
			t.Fatalf("secret leaked to persisted state or witness: %s", secret)
		}
	}
	if runtime.GOOS != "windows" {
		for _, protectedPath := range []string{path, witnessPath, markerPath} {
			info, statErr := os.Stat(protectedPath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("unexpected permissions for %s: %o", protectedPath, info.Mode().Perm())
			}
		}
	}

	var legacy state
	if err := json.Unmarshal(candidateState, &legacy); err != nil {
		t.Fatal(err)
	}
	artifact := &legacy.Changes[0].Artifacts[0]
	artifact.ID = "artifact_legacy_random"
	artifact.ContentSHA256 = ""
	legacy.Changes[0].SQLSHA256 = ""
	legacy.Changes[0].RollbackSHA256 = ""
	legacy.Changes[0].ArtifactSHA256 = ""
	writeStateFixture(t, path, legacy)

	second, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	secondStatus := second.MigrationWitnessStatus()
	second.Close()
	if secondStatus.Reconciliation != "external-state-rehydrated" || secondStatus.RestoredChanges != 1 || secondStatus.RestoredArtifacts != 1 {
		t.Fatalf("legacy state was not rehydrated from the witness: %+v", secondStatus)
	}
	if after := readFileFixture(t, path); !bytes.Equal(candidateState, after) {
		t.Fatalf("candidate state did not converge after legacy rollback: %s", normalizationDifference(candidateState, after))
	}
	if after := readFileFixture(t, witnessPath); !bytes.Equal(witnessBefore, after) {
		t.Fatal("migration witness changed after an equivalent rollback round trip")
	}

	var restored state
	if err := json.Unmarshal(candidateState, &restored); err != nil {
		t.Fatal(err)
	}
	restoredChange := restored.Changes[0]
	restoredArtifact := restoredChange.Artifacts[0]
	if restoredArtifact.ID != "artifact_stable" || restoredArtifact.ContentSHA256 != changegate.SHA256(rawArtifact) {
		t.Fatalf("artifact integrity evidence mismatch: %+v", restoredArtifact)
	}
	if restoredChange.SQLSHA256 != changegate.SHA256(rawSQL) || restoredChange.RollbackSHA256 != changegate.SHA256(rawRollback) {
		t.Fatalf("change integrity evidence mismatch: %+v", restoredChange)
	}
}

func TestFileMigrationWitnessFailsClosedWhenRequiredFileIsMissing(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "false")
	t.Setenv("DBGUARD_MIGRATION_WITNESS_FILE", "")
	path := filepath.Join(t.TempDir(), "dbguard.json")
	writeStateFixture(t, path, migrationWitnessFixture())
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := os.Remove(path + ".rollback-witness.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "required but missing") {
		t.Fatalf("missing required witness did not fail closed: %v", err)
	}
}

func TestFileMigrationWitnessRejectsTampering(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "false")
	t.Setenv("DBGUARD_MIGRATION_WITNESS_FILE", "")
	path := filepath.Join(t.TempDir(), "dbguard.json")
	writeStateFixture(t, path, migrationWitnessFixture())
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	witnessPath := path + ".rollback-witness.json"
	var document map[string]any
	if err := json.Unmarshal(readFileFixture(t, witnessPath), &document); err != nil {
		t.Fatal(err)
	}
	document["payload_sha256"] = strings.Repeat("0", 64)
	tampered, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(witnessPath, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "payload digest mismatch") {
		t.Fatalf("tampered witness did not fail closed: %v", err)
	}
}

func TestFileMigrationWitnessRecoversInterruptedStateWrite(t *testing.T) {
	t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "false")
	t.Setenv("DBGUARD_MIGRATION_WITNESS_FILE", "")
	path := filepath.Join(t.TempDir(), "dbguard.json")
	writeStateFixture(t, path, migrationWitnessFixture())
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	committedContent := readFileFixture(t, path)
	var committed state
	if err := json.Unmarshal(committedContent, &committed); err != nil {
		t.Fatal(err)
	}
	committedSnapshot, err := buildMigrationWitnessSnapshot(committed, committedContent)
	if err != nil {
		t.Fatal(err)
	}
	pending := committed
	pending.Changes[0].Description = "write that never reached the state file"
	pendingContent, err := json.MarshalIndent(pending, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	pendingSnapshot, err := buildMigrationWitnessSnapshot(pending, pendingContent)
	if err != nil {
		t.Fatal(err)
	}
	witnessPath := path + ".rollback-witness.json"
	markerPath := witnessPath + ".required"
	if err := persistMigrationWitness(witnessPath, markerPath, pendingSnapshot, committedSnapshot); err != nil {
		t.Fatal(err)
	}

	recovered, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	status := recovered.MigrationWitnessStatus()
	recovered.Close()
	if status.Reconciliation != "interrupted-save-recovered" || !status.InterruptedSaveUsed {
		t.Fatalf("interrupted save was not recovered: %+v", status)
	}
	if after := readFileFixture(t, path); !bytes.Equal(committedContent, after) {
		t.Fatalf("interrupted witness replaced committed state: %s", normalizationDifference(committedContent, after))
	}
}

func TestMigrationWitnessPathCannotReplaceDataFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dbguard.json")
	t.Setenv("DBGUARD_MIGRATION_WITNESS_FILE", path)
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("unsafe witness path was accepted: %v", err)
	}
}

func TestMigrationWitnessCanonicalDigestMatchesBackupVerifier(t *testing.T) {
	fixture := []byte(`{
  "schema": "changeguard-migration-witness/v1",
  "current": {
    "state_sha256": "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a",
    "changes": [],
    "artifacts": []
  },
  "payload_sha256": "93bbd870b37072df0503d404ebfd172d66630939c057114ace6110d9aebb807e"
}`)
	if _, err := decodeMigrationWitness(fixture); err != nil {
		t.Fatalf("backup verifier canonical digest fixture was rejected: %v", err)
	}
}

func migrationWitnessFixture() state {
	return state{
		Organizations: []model.Organization{{ID: "org_prod", Name: "Production"}},
		Changes: []model.ChangeRequest{{
			ID: "chg_fixture", OrganizationID: "org_prod", Environment: "生产", ChangeType: "配置变更",
			Artifacts: []model.ChangeArtifact{{ID: "artifact_fixture", Kind: model.ArtifactConfig, Name: "fixture.yaml", Content: "enabled: true\n"}},
		}},
	}
}

func writeStateFixture(t *testing.T, path string, value state) {
	t.Helper()
	content, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFileFixture(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return content
}
