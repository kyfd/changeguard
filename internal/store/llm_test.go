package store

import (
	"errors"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func newModelConfigStore(t *testing.T) *Store {
	t.Helper()
	path := t.TempDir() + "/dbguard.json"
	store, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func auditFor(org, actor string) model.AuditEvent {
	return model.AuditEvent{
		OrganizationID: org,
		ActorID:        actor,
		ActorName:      actor,
		Action:         "test.model_config",
		CreatedAt:      time.Now().UTC(),
	}
}

func TestModelConfigIsolatedPerOrganization(t *testing.T) {
	store := newModelConfigStore(t)

	saved, err := store.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.Enabled = true
		c.Provider = model.ModelProviderOpenAI
		c.BaseURL = "https://api.deepseek.com"
		c.Model = "deepseek-chat"
		c.APIKeyCipher = "v1:ciphertext-a"
		c.APIKeyHint = "已保存 sk-…aaaa"
		return nil
	}, auditFor("org_a", "usr_a"))
	if err != nil {
		t.Fatalf("UpsertModelConfig: %v", err)
	}
	if saved.OrganizationID != "org_a" {
		t.Fatalf("organization id = %q", saved.OrganizationID)
	}
	if saved.ConfiguredBy != "usr_a" {
		t.Fatalf("configured_by = %q", saved.ConfiguredBy)
	}
	if saved.ConfiguredAt.IsZero() || saved.UpdatedAt.IsZero() {
		t.Fatal("configured_at/updated_at must be stamped")
	}

	// 另一个组织读不到：这是隔离的核心断言。
	other, found := store.ModelConfig("org_b")
	if found {
		t.Fatalf("org_b unexpectedly sees a config: %+v", other)
	}

	// 同组织读到的就是自己那份。
	got, found := store.ModelConfig("org_a")
	if !found {
		t.Fatal("org_a must see its own config")
	}
	if got.Model != "deepseek-chat" || got.APIKeyCipher != "v1:ciphertext-a" {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// 同组织内更新应覆盖，而不是新增一条。
	if _, err := store.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.Model = "deepseek-reasoner"
		return nil
	}, auditFor("org_a", "usr_a")); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	all := store.ModelConfigs()
	if len(all) != 1 {
		t.Fatalf("expected 1 config after an in-org update, got %d", len(all))
	}
	if all[0].Model != "deepseek-reasoner" {
		t.Fatalf("update did not apply: %+v", all[0])
	}
}

// 更新函数返回 error 时必须整体拒绝，且不留下半改的状态。
func TestUpsertModelConfigRejectsFailedUpdate(t *testing.T) {
	store := newModelConfigStore(t)
	wantErr := errors.New("reject this change")

	if _, err := store.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.Model = "should-not-persist"
		return wantErr
	}, auditFor("org_a", "usr_a")); err == nil {
		t.Fatal("expected the update error to surface")
	}

	if _, found := store.ModelConfig("org_a"); found {
		t.Fatal("a rejected update must not create a record")
	}
	if len(store.ModelConfigs()) != 0 {
		t.Fatal("a rejected update must not leave a partial record")
	}
}

// 更新不能改写 organization_id：调用方传错组织时不能把配置搬到别处。
func TestUpsertModelConfigCannotMoveOrganization(t *testing.T) {
	store := newModelConfigStore(t)
	if _, err := store.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.Model = "m"
		return nil
	}, auditFor("org_a", "usr_a")); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	if _, err := store.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.OrganizationID = "org_b"
		return nil
	}, auditFor("org_a", "usr_a")); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, found := store.ModelConfig("org_a")
	if !found {
		t.Fatal("org_a lost its config")
	}
	if got.OrganizationID != "org_a" {
		t.Fatalf("organization was moved to %q", got.OrganizationID)
	}
	if _, found := store.ModelConfig("org_b"); found {
		t.Fatal("org_b must not have a config")
	}
}

// 配置必须落盘：重启后应仍然读得到（含密文）。
func TestModelConfigSurvivesReload(t *testing.T) {
	path := t.TempDir() + "/dbguard.json"
	first, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := first.UpsertModelConfig("org_a", "usr_a", func(c *model.OrganizationModelConfig) error {
		c.Enabled = true
		c.Model = "deepseek-chat"
		c.APIKeyCipher = "v1:persisted-ciphertext"
		return nil
	}, auditFor("org_a", "usr_a")); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	first.Close()

	second, err := New(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { second.Close() }()

	got, found := second.ModelConfig("org_a")
	if !found {
		t.Fatal("config did not survive a reload")
	}
	if got.APIKeyCipher != "v1:persisted-ciphertext" {
		t.Fatalf("ciphertext changed across reload: %q", got.APIKeyCipher)
	}
}
