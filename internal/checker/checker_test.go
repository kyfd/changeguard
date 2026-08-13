package checker

import (
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestSplitStatementsKeepsQuotedSemicolon(t *testing.T) {
	items := SplitStatements("INSERT INTO notes(value) VALUES ('a;b'); UPDATE notes SET value='c' WHERE id=1;")
	if len(items) != 2 {
		t.Fatalf("expected 2 statements, got %d: %#v", len(items), items)
	}
}

func TestCheckFindsUnsafeUpdate(t *testing.T) {
	result := Check("UPDATE orders SET status='done';", "UPDATE orders SET status='pending' WHERE id=:id;")
	if result.Risk != model.RiskHigh {
		t.Fatalf("expected high risk, got %s", result.Risk)
	}
	if result.Findings[0].Code != "UPDATE_WITHOUT_WHERE" {
		t.Fatalf("unexpected finding: %#v", result.Findings)
	}
}

func TestCheckUniqueIndexRequiresAttention(t *testing.T) {
	result := Check("CREATE UNIQUE INDEX ux_orders_request_id ON orders(request_id);", "DROP INDEX IF EXISTS ux_orders_request_id;")
	if result.Risk != model.RiskMedium {
		t.Fatalf("expected medium risk, got %s", result.Risk)
	}
	if len(result.Findings) < 2 {
		t.Fatalf("expected index findings, got %#v", result.Findings)
	}
}

func TestDisabledPolicyNoLongerBlocksSubmission(t *testing.T) {
	policies := model.DefaultRiskPolicies(time.Now())
	for index := range policies {
		if policies[index].Code == "UPDATE_WITHOUT_WHERE" {
			policies[index].Enabled = false
		}
	}
	result := CheckWithPolicies(
		"UPDATE orders SET status='done';",
		"UPDATE orders SET status='pending' WHERE id=:id;",
		Context{Environment: "生产环境", ChangeType: "DML"},
		policies,
	)
	for _, finding := range result.Findings {
		if finding.Code == "UPDATE_WITHOUT_WHERE" {
			t.Fatalf("disabled policy still matched: %#v", finding)
		}
	}
	if result.Risk != model.RiskLow {
		t.Fatalf("expected low risk after disabling policy, got %s", result.Risk)
	}
}

func TestCustomPolicyHonorsScopeAndVersion(t *testing.T) {
	policy := model.RiskPolicy{
		ID: "pol_select_star", Code: "SELECT_STAR", Name: "禁止查询全部字段",
		Description: "生产查询不得直接使用星号。", Pattern: `(?i)\bSELECT\s+\*\s+FROM\b`,
		Severity: model.RiskMedium, Blocking: true, Enabled: true, Version: 3,
		Environments: []string{"生产环境"}, ChangeTypes: []string{"DML"},
		Suggestion: "显式列出需要的字段。",
	}
	result := CheckWithPolicies(
		"SELECT * FROM orders;",
		"SELECT id FROM orders;",
		Context{Environment: "生产环境", ChangeType: "DML"},
		[]model.RiskPolicy{policy},
	)
	if len(result.Findings) != 1 || result.Findings[0].Code != policy.Code {
		t.Fatalf("expected custom policy finding, got %#v", result.Findings)
	}
	if !result.Findings[0].Blocking || result.Findings[0].RuleVersion != 3 {
		t.Fatalf("expected blocking versioned finding, got %#v", result.Findings[0])
	}
	skipped := CheckWithPolicies(
		"SELECT * FROM orders;",
		"SELECT id FROM orders;",
		Context{Environment: "测试环境", ChangeType: "DML"},
		[]model.RiskPolicy{policy},
	)
	for _, finding := range skipped.Findings {
		if finding.Code == policy.Code {
			t.Fatalf("scoped policy matched the wrong environment")
		}
	}
}

func TestTransactionControlAndPSQLMetaCommandsAreBlocked(t *testing.T) {
	for _, item := range []struct {
		sql  string
		code string
	}{
		{sql: "COMMIT;", code: "TRANSACTION_CONTROL"},
		{sql: "\\! id", code: "PSQL_META_COMMAND"},
	} {
		result := Check(item.sql, "SELECT 1;")
		found := false
		for _, finding := range result.Findings {
			if finding.Code == item.code && finding.Blocking && finding.Severity == model.RiskHigh {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected blocking finding %s for %q: %+v", item.code, item.sql, result.Findings)
		}
	}
}

func TestReleaseCheckBlocksUnsafeKubernetesManifest(t *testing.T) {
	result := CheckReleaseWithPolicies(ReleaseInput{
		RollbackPlan: "恢复上一版本镜像并切回稳定流量。",
		ReleasePlan: model.ReleasePlan{
			Strategy:           "金丝雀发布",
			CanaryPercent:      10,
			ObservationMinutes: 15,
			SuccessMetrics:     []string{"HTTP 5xx", "P99 延迟"},
		},
		Artifacts: []model.ChangeArtifact{{
			Kind: model.ArtifactKubernetes,
			Name: "deployment.yaml",
			Content: `image: example/order:latest
securityContext:
  privileged: true`,
		}},
	}, Context{Environment: "生产环境", ChangeType: "Kubernetes 发布"}, model.DefaultRiskPolicies(time.Now()))

	for _, code := range []string{"K8S_LATEST_IMAGE", "K8S_PRIVILEGED", "K8S_RESOURCE_LIMITS"} {
		if !containsFinding(result.Findings, code) {
			t.Fatalf("expected %s finding, got %+v", code, result.Findings)
		}
	}
	if result.Risk != model.RiskHigh {
		t.Fatalf("expected high risk, got %s", result.Risk)
	}
}

func TestReleaseCheckFindsPlaintextSecret(t *testing.T) {
	result := CheckReleaseWithPolicies(ReleaseInput{
		RollbackPlan: "恢复上一版本配置。",
		ReleasePlan: model.ReleasePlan{
			Strategy:           "分批发布",
			CanaryPercent:      20,
			ObservationMinutes: 10,
			SuccessMetrics:     []string{"配置加载成功率"},
		},
		Artifacts: []model.ChangeArtifact{{
			Kind:    model.ArtifactConfig,
			Name:    "application.yaml",
			Content: "password: super-secret\ndebug: true",
		}},
	}, Context{Environment: "生产环境", ChangeType: "配置变更"}, model.DefaultRiskPolicies(time.Now()))

	if !containsFinding(result.Findings, "CONFIG_SECRET_EXPOSURE") {
		t.Fatalf("expected plaintext secret finding, got %+v", result.Findings)
	}
	if !containsFinding(result.Findings, "CONFIG_DEBUG_ENABLED") {
		t.Fatalf("expected debug configuration finding, got %+v", result.Findings)
	}
}

func TestSafeReleaseProducesNoSyntheticFinding(t *testing.T) {
	result := CheckReleaseWithPolicies(ReleaseInput{
		RollbackPlan: "将流量切回上一稳定版本并保留现场日志。",
		ReleasePlan: model.ReleasePlan{
			Strategy:           "金丝雀发布",
			CanaryPercent:      10,
			ObservationMinutes: 15,
			AutoRollback:       true,
			SuccessMetrics:     []string{"HTTP 5xx", "P99 延迟", "订单成功率"},
		},
		Artifacts: []model.ChangeArtifact{{
			Kind:    model.ArtifactConfig,
			Name:    "application.yaml",
			Content: "debug: false\nauth_enabled: true",
		}},
	}, Context{Environment: "生产环境", ChangeType: "配置变更"}, model.DefaultRiskPolicies(time.Now()))

	if len(result.Findings) != 0 {
		t.Fatalf("safe release must not persist a synthetic pass finding, got %+v", result.Findings)
	}
	if result.Risk != model.RiskLow {
		t.Fatalf("expected low risk, got %s", result.Risk)
	}
}

func TestTransactionOptimizationRules(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		code string
	}{
		{
			name: "unbatched update",
			sql:  "UPDATE orders SET status='archived' WHERE created_at < NOW() - INTERVAL '90 days';",
			code: "UNBATCHED_LARGE_DML",
		},
		{
			name: "fk without not valid",
			sql:  "ALTER TABLE inventory_reservation ADD CONSTRAINT fk_reservation_sku FOREIGN KEY (sku_id) REFERENCES sku(id);",
			code: "FK_WITHOUT_NOT_VALID",
		},
		{
			name: "for update unbounded",
			sql:  "SELECT * FROM orders WHERE status='PENDING' FOR UPDATE;",
			code: "SELECT_FOR_UPDATE_UNBOUNDED",
		},
		{
			name: "heavy ddl rewrite",
			sql:  "VACUUM FULL orders;",
			code: "HEAVY_DDL_REWRITE",
		},
		{
			name: "missing lock timeout on ddl",
			sql:  "CREATE INDEX idx_orders_status ON orders(status);",
			code: "MISSING_LOCK_TIMEOUT",
		},
		{
			name: "mixed ddl and bulk dml",
			sql:  "ALTER TABLE orders ADD COLUMN note text; UPDATE orders SET note='x' WHERE id > 0;",
			code: "MIXED_DDL_DML_TRANSACTION",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			result := Check(item.sql, "SELECT 1;")
			if !containsFinding(result.Findings, item.code) {
				t.Fatalf("expected %s, got %+v", item.code, codesOf(result.Findings))
			}
		})
	}
}

func TestBatchedDMLWithTimeoutIsClean(t *testing.T) {
	sql := `
SET LOCAL lock_timeout = '2s';
SET LOCAL statement_timeout = '30s';
UPDATE member_points SET status='EXPIRED'
WHERE expires_at < NOW() - INTERVAL '7 days' AND status='ACTIVE'
LIMIT 10000;
`
	result := Check(sql, "UPDATE member_points SET status='ACTIVE' WHERE status='EXPIRED' AND updated_at >= NOW() - INTERVAL '1 hour' LIMIT 10000;")
	for _, code := range []string{"UNBATCHED_LARGE_DML", "MISSING_LOCK_TIMEOUT", "UPDATE_WITHOUT_WHERE"} {
		if containsFinding(result.Findings, code) {
			t.Fatalf("batched DML with timeouts should not hit %s: %+v", code, codesOf(result.Findings))
		}
	}
}

func TestForeignKeyNotValidIsAccepted(t *testing.T) {
	sql := `
SET LOCAL lock_timeout = '2s';
ALTER TABLE inventory_reservation
  ADD CONSTRAINT fk_reservation_sku FOREIGN KEY (sku_id) REFERENCES sku(id) NOT VALID;
`
	result := Check(sql, "ALTER TABLE inventory_reservation DROP CONSTRAINT IF EXISTS fk_reservation_sku;")
	if containsFinding(result.Findings, "FK_WITHOUT_NOT_VALID") {
		t.Fatalf("NOT VALID foreign key must not be flagged: %+v", codesOf(result.Findings))
	}
}

func codesOf(findings []model.Finding) []string {
	codes := make([]string, 0, len(findings))
	for _, finding := range findings {
		codes = append(codes, finding.Code)
	}
	return codes
}

func containsFinding(findings []model.Finding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
