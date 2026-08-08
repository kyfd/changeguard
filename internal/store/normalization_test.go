package store

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestNormalizeStateIsRestartIdempotent(t *testing.T) {
	for _, demoEnabled := range []string{"false", "true"} {
		t.Run("demo_data_"+demoEnabled, func(t *testing.T) {
			t.Setenv("DBGUARD_ENABLE_DEMO_DATA", demoEnabled)
			data := seedState()
			normalizeState(&data)
			first, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			normalizeState(&data)
			second, err := json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatalf("state normalization changed already-normalized data: %s", normalizationDifference(first, second))
			}
		})
	}
}

func normalizationDifference(first, second []byte) string {
	var left, right map[string]json.RawMessage
	if json.Unmarshal(first, &left) != nil || json.Unmarshal(second, &right) != nil {
		return "unable to decode diagnostic JSON"
	}
	changed := make([]string, 0)
	for key, value := range left {
		if !bytes.Equal(value, right[key]) {
			changed = append(changed, key)
		}
	}
	sort.Strings(changed)
	detail := "changed top-level collections=" + strings.Join(changed, ",")
	if slicesContain(changed, "changes") {
		var leftChanges, rightChanges []map[string]any
		if json.Unmarshal(left["changes"], &leftChanges) == nil && json.Unmarshal(right["changes"], &rightChanges) == nil {
			rightByID := make(map[string]map[string]any, len(rightChanges))
			for _, change := range rightChanges {
				rightByID[change["id"].(string)] = change
			}
			changedIDs := make([]string, 0)
			for _, change := range leftChanges {
				id := change["id"].(string)
				if !equalJSON(change, rightByID[id]) {
					changedIDs = append(changedIDs, id)
				}
			}
			sort.Strings(changedIDs)
			detail += " changed change IDs=" + strings.Join(changedIDs, ",")
		}
	}
	return detail
}

func slicesContain(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func equalJSON(left, right any) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return bytes.Equal(leftJSON, rightJSON)
}

func TestMigrationIdentifiersAreDeterministicAndScoped(t *testing.T) {
	artifact := migratedArtifactID("chg_1", "config")
	if artifact != migratedArtifactID("chg_1", "config") {
		t.Fatal("artifact migration identifier is not deterministic")
	}
	if artifact == migratedArtifactID("chg_2", "config") || artifact == migratedArtifactID("chg_1", "kubernetes") {
		t.Fatal("artifact migration identifier is not scoped to change and role")
	}
	if !strings.HasPrefix(artifact, "artifact_migrated_") {
		t.Fatalf("unexpected artifact migration identifier: %s", artifact)
	}

	policy := migratedPolicyID("org_1", "POLICY_A")
	if policy != migratedPolicyID("org_1", "POLICY_A") {
		t.Fatal("policy migration identifier is not deterministic")
	}
	if policy == migratedPolicyID("org_2", "POLICY_A") || policy == migratedPolicyID("org_1", "POLICY_B") {
		t.Fatal("policy migration identifier is not scoped to organization and code")
	}
	if !strings.HasPrefix(policy, "pol_migrated_") {
		t.Fatalf("unexpected policy migration identifier: %s", policy)
	}
}

func TestDemoEnrichmentRequiresExplicitEnablement(t *testing.T) {
	buildState := func() state {
		return state{
			Organizations: []model.Organization{{ID: "org_demo", Name: "Legacy production organization"}},
			Changes: []model.ChangeRequest{{
				ID: "chg_20260730_002", OrganizationID: "org_demo", ChangeType: "CONFIG",
				Artifacts: []model.ChangeArtifact{{ID: "artifact_original", Kind: model.ArtifactConfig, Content: "enabled: false"}},
			}},
		}
	}

	t.Run("disabled", func(t *testing.T) {
		t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "false")
		data := buildState()
		normalizeState(&data)
		if len(data.Changes) != 1 || len(data.Changes[0].Artifacts) != 1 || data.Changes[0].Artifacts[0].ID != "artifact_original" {
			t.Fatal("disabled demo data unexpectedly rewrote legacy artifacts")
		}
	})

	t.Run("enabled", func(t *testing.T) {
		t.Setenv("DBGUARD_ENABLE_DEMO_DATA", "true")
		data := buildState()
		normalizeState(&data)
		if len(data.Changes) == 0 || len(data.Changes[0].Artifacts) != 1 || data.Changes[0].Artifacts[0].ID != migratedArtifactID("chg_20260730_002", "inventory-kubernetes") {
			t.Fatal("enabled demo data did not apply deterministic enrichment")
		}
	})
}
