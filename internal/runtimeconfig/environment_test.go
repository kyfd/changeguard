package runtimeconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRejectsDuplicateCanonicalKeyWithoutLeakingValues(t *testing.T) {
	clearEnvironment(t)
	path := writeEnvironment(t, "DBGUARD_EXPERIMENT_MODE=secret-one\nDBGUARD_EXPERIMENT_MODE=secret-two\n")
	t.Setenv("DBGUARD_ENV_FILE", path)

	_, err := Load()
	if err == nil {
		t.Fatal("expected duplicate key rejection")
	}
	message := err.Error()
	if !strings.Contains(message, "duplicate environment key DBGUARD_EXPERIMENT_MODE") || !strings.Contains(message, "lines 1 and 2") {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(message, "secret-one") || strings.Contains(message, "secret-two") {
		t.Fatalf("configuration error leaked a value: %v", err)
	}
}

func TestLoadRejectsConflictingInheritedOverride(t *testing.T) {
	clearEnvironment(t)
	path := writeEnvironment(t, "DBGUARD_WORKERS=2\n")
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_WORKERS", "9")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "canonical environment conflict for DBGUARD_WORKERS") {
		t.Fatalf("expected canonical override rejection, got %v", err)
	}
}

func TestLoadRejectsMissingExplicitFile(t *testing.T) {
	clearEnvironment(t)
	t.Setenv("DBGUARD_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "load canonical environment file") {
		t.Fatalf("expected missing explicit file error, got %v", err)
	}
}

func TestLoadAcceptsCompleteProductionFile(t *testing.T) {
	clearEnvironment(t)
	path := writeEnvironment(t, validProductionEnvironment())
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_ENV_PROFILE", ProfileProduction)

	summary, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if summary.Profile != ProfileProduction || !summary.Explicit || summary.Assignments < 15 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	if got := os.Getenv("DBGUARD_EXPERIMENT_MODE"); got != "postgres" {
		t.Fatalf("experiment mode = %q", got)
	}
}

func TestProductionProfileRejectsDemoOnlyExperiment(t *testing.T) {
	clearEnvironment(t)
	content := strings.Replace(validProductionEnvironment(), "DBGUARD_EXPERIMENT_MODE=postgres", "DBGUARD_EXPERIMENT_MODE=demo_only", 1)
	path := writeEnvironment(t, content)
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_ENV_PROFILE", ProfileProduction)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DBGUARD_EXPERIMENT_MODE must be postgres") {
		t.Fatalf("expected demo_only rejection, got %v", err)
	}
}

func TestProductionProfileRejectsRelativeWitnessPath(t *testing.T) {
	clearEnvironment(t)
	content := strings.Replace(validProductionEnvironment(), "/var/lib/changeguard/dbguard.json.rollback-witness.json", "dbguard.json.rollback-witness.json", 1)
	path := writeEnvironment(t, content)
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_ENV_PROFILE", ProfileProduction)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DBGUARD_MIGRATION_WITNESS_FILE must be an explicit absolute path") {
		t.Fatalf("expected witness path rejection, got %v", err)
	}
}

func TestProductionURLErrorDoesNotLeakSecret(t *testing.T) {
	clearEnvironment(t)
	content := strings.Replace(validProductionEnvironment(), "redis://session-user:session-password@127.0.0.1:6379/3", "not-a-url-with-super-secret", 1)
	path := writeEnvironment(t, content)
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_ENV_PROFILE", ProfileProduction)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DBGUARD_REDIS_URL must be a valid absolute URL") {
		t.Fatalf("expected Redis URL rejection, got %v", err)
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("URL error leaked secret material: %v", err)
	}
}

func TestProductionProfileRejectsLongPlaceholderSecret(t *testing.T) {
	clearEnvironment(t)
	content := strings.Replace(validProductionEnvironment(), "metrics-token-at-least-32-bytes-long", "REPLACE_ME_WITH_A_64_BYTE_METRICS_TOKEN________________", 1)
	path := writeEnvironment(t, content)
	t.Setenv("DBGUARD_ENV_FILE", path)
	t.Setenv("DBGUARD_ENV_PROFILE", ProfileProduction)

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DBGUARD_METRICS_TOKEN must not use a placeholder value") {
		t.Fatalf("expected placeholder rejection, got %v", err)
	}
}

func writeEnvironment(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "core.env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func clearEnvironment(t *testing.T) {
	t.Helper()
	keys := []string{
		"DBGUARD_ENV_FILE", "DBGUARD_ENV_PROFILE", "DBGUARD_ENABLE_DEMO_ACCOUNTS", "DBGUARD_ENABLE_DEMO_DATA",
		"DBGUARD_AUTH_SECURE_COOKIE", "DBGUARD_TRUST_PROXY_HEADERS", "DBGUARD_PUBLIC_URL", "DBGUARD_AUTH_MODE",
		"DBGUARD_OIDC_ISSUER", "DBGUARD_OIDC_REDIRECT_URL", "DBGUARD_OIDC_CLIENT_ID", "DBGUARD_OIDC_CLIENT_SECRET",
		"DBGUARD_SESSION_MODE", "DBGUARD_REDIS_URL", "DBGUARD_EXPERIMENT_MODE", "DBGUARD_SHADOW_DSN",
		"DBGUARD_PASSPORT_HMAC_SECRET", "DBGUARD_METRICS_TOKEN", "DBGUARD_OPERATIONS_WEBHOOK_TOKEN",
		"DBGUARD_OPERATIONS_ORGANIZATION_ID", "DBGUARD_STORE_MODE", "DBGUARD_DATA_FILE",
		"DBGUARD_MIGRATION_WITNESS_FILE", "DBGUARD_PRIMARY_DSN", "DBGUARD_WORKERS",
	}
	for _, key := range keys {
		value, exists := os.LookupEnv(key)
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
		key, value, exists := key, value, exists
		t.Cleanup(func() {
			if exists {
				_ = os.Setenv(key, value)
			} else {
				_ = os.Unsetenv(key)
			}
		})
	}
}

func validProductionEnvironment() string {
	return strings.Join([]string{
		"DBGUARD_AUTH_MODE=local",
		"DBGUARD_AUTH_SECURE_COOKIE=true",
		"DBGUARD_TRUST_PROXY_HEADERS=true",
		"DBGUARD_PUBLIC_URL=https://changeguard.example.com",
		"DBGUARD_ENABLE_DEMO_ACCOUNTS=false",
		"DBGUARD_ENABLE_DEMO_DATA=false",
		"DBGUARD_SESSION_MODE=redis",
		"DBGUARD_REDIS_URL=redis://session-user:session-password@127.0.0.1:6379/3",
		"DBGUARD_EXPERIMENT_MODE=postgres",
		"DBGUARD_SHADOW_DSN=postgres://shadow-user:shadow-password@127.0.0.1:5432/changeguard_shadow?sslmode=require",
		"DBGUARD_PASSPORT_HMAC_SECRET=passport-secret-at-least-32-bytes-long",
		"DBGUARD_METRICS_TOKEN=metrics-token-at-least-32-bytes-long",
		"DBGUARD_OPERATIONS_WEBHOOK_TOKEN=operations-token-at-least-32-bytes",
		"DBGUARD_OPERATIONS_ORGANIZATION_ID=org_enterprise",
		"DBGUARD_STORE_MODE=file",
		"DBGUARD_DATA_FILE=/var/lib/changeguard/dbguard.json",
		"DBGUARD_MIGRATION_WITNESS_FILE=/var/lib/changeguard/dbguard.json.rollback-witness.json",
		"DBGUARD_WORKERS=2",
	}, "\n") + "\n"
}
