package service

import (
	"errors"
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

func seedPassport(t *testing.T, data *store.Store, change model.ChangeRequest, id string) model.Passport {
	t.Helper()
	now := time.Now().UTC()
	passport := model.Passport{
		ID: id, OrganizationID: change.OrganizationID, ChangeID: change.ID,
		ArtifactSHA256: change.ArtifactSHA256, Environment: change.Environment,
		RuleSetVersion: changegate.RuleSetVersion(data.PoliciesByOrganization(change.OrganizationID)),
		ApproverID:     "usr_reviewer", Status: model.PassportActive,
		TokenSHA256: "token-hash-" + id, IssuedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if passport.ArtifactSHA256 == "" {
		passport.ArtifactSHA256 = "artifact-" + change.ID
	}
	if passport.Environment == "" {
		passport.Environment = "生产环境"
	}
	if err := data.CreatePassport(passport, model.AuditEvent{
		OrganizationID: change.OrganizationID, ID: "audit_" + id, ChangeID: change.ID, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return passport
}

func TestPassportsForIsolatesTenantsAndOmitsTokenHash(t *testing.T) {
	data := store.NewMemory()
	now := time.Now()
	organization := model.Organization{
		ID: "org_isolated", Name: "隔离测试企业", Slug: "isolated",
		EmailDomains: []string{"isolated.example"}, CreatedBy: "usr_isolated",
		CreatedAt: now, UpdatedAt: now,
	}
	user := model.User{
		ID: "usr_isolated", OrganizationID: organization.ID, OrganizationName: organization.Name,
		Name: "隔离用户", Email: "owner@isolated.example", Role: model.RoleOwner,
		EnterpriseAdmin: true, Active: true,
	}
	policies := model.DefaultRiskPolicies(now)
	for index := range policies {
		policies[index].ID = store.NewID("pol_")
		policies[index].OrganizationID = organization.ID
	}
	if err := data.CreateEnterprise(organization, user, model.UserCredential{UserID: user.ID}, policies, model.AuditEvent{
		OrganizationID: organization.ID, ID: "audit_isolated_passports", ActorID: user.ID,
		ActorName: user.Name, Action: "REGISTER_ENTERPRISE", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	svc := New(data, fakeRunner{}, fakeAnalyzer{})

	demoChange := data.Changes()[0]
	seedPassport(t, data, demoChange, "pass_demo_visible")

	items, err := svc.PassportsFor(user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("new enterprise must not see demo passports: %+v", items)
	}
	if _, err := svc.PassportsForChange(demoChange.ID, user.ID); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-tenant passport read must be forbidden, got %v", err)
	}

	ownerItems, err := svc.PassportsFor("usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range ownerItems {
		if item.ID == "pass_demo_visible" {
			found = true
			if item.TokenSHA256 != "" {
				t.Fatalf("list endpoint must not expose token hash: %+v", item)
			}
		}
		if item.OrganizationID != "org_demo" {
			t.Fatalf("owner list leaked foreign passport: %+v", item)
		}
	}
	if !found {
		t.Fatal("demo owner must see the seeded passport")
	}
}

func TestPassportsForHonorsApplicationGrants(t *testing.T) {
	data := store.NewMemory()
	svc := New(data, fakeRunner{}, fakeAnalyzer{})
	orderChange, err := svc.Create(model.CreateChangeInput{
		Title: "订单通行证", ApplicationID: "app_order", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts: []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	inventoryChange, err := svc.Create(model.CreateChangeInput{
		Title: "库存通行证", ApplicationID: "app_inventory", ChangeType: "配置变更", Environment: "生产环境",
		Artifacts: []model.ChangeArtifact{{Kind: model.ArtifactConfig, Name: "app.yaml", Content: "debug: false\nauth_enabled: true\ntls_verify: true"}},
	}, "usr_developer")
	if err != nil {
		t.Fatal(err)
	}
	seedPassport(t, data, orderChange, "pass_order")
	seedPassport(t, data, inventoryChange, "pass_inventory")

	if _, err = data.UpdateMember("org_demo", "usr_reviewer", func(user *model.User) error { return nil }, []model.ApplicationGrantInput{{ApplicationID: "app_order", CanReview: true}}, model.AuditEvent{OrganizationID: "org_demo", ID: "audit_passport_grant", ActorID: "usr_owner", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	items, err := svc.PassportsFor("usr_reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != "pass_order" {
		t.Fatalf("reviewer without inventory grant must only see order passport: %+v", items)
	}

	ownerItems, err := svc.PassportsFor("usr_owner")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, item := range ownerItems {
		seen[item.ID] = true
	}
	if !seen["pass_order"] || !seen["pass_inventory"] {
		t.Fatalf("owner must see both passports: %+v", ownerItems)
	}
}
