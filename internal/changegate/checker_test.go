package changegate

import (
	"strings"
	"testing"
)

func detectionCodeSet(items []Detection) map[string]bool {
	result := make(map[string]bool, len(items))
	for _, item := range items {
		result[item.Code] = true
	}
	return result
}

func TestCheckConfigDetectsRedactedSecretAndUnsafeProductionFlags(t *testing.T) {
	findings := CheckConfig("api_key: [REDACTED]\ndebug: true\nauth_enabled: false\ninsecure_skip_verify: true", "application.yaml", true)
	codes := detectionCodeSet(findings)
	for _, code := range []string{"CONFIG_SECRET_EXPOSURE", "CONFIG_DEBUG_ENABLED", "CONFIG_AUTH_DISABLED", "CONFIG_TLS_VERIFY_DISABLED"} {
		if !codes[code] {
			t.Fatalf("expected %s, got %+v", code, findings)
		}
	}
}

func TestCheckConfigAllowsSecretReferencesAndSafeInverses(t *testing.T) {
	findings := CheckConfig("api_key: vault:secret/sms\nauth_enabled: true\ndisable_auth: false\nallow_anonymous: false\ninsecure_skip_verify: false\ntls_verify: true\ndebug: false", "application.yaml", true)
	if len(findings) != 0 {
		t.Fatalf("safe references and inverse flags must not be blocked: %+v", findings)
	}
}

func TestCheckKubernetesIgnoresServiceDocuments(t *testing.T) {
	manifest := "apiVersion: v1\nkind: Service\nmetadata:\n  name: api\nspec:\n  selector:\n    app: api\n  ports:\n    - port: 80"
	if findings := CheckKubernetes(manifest, "service.yaml", true); len(findings) != 0 {
		t.Fatalf("Service has no pod spec and must not receive workload findings: %+v", findings)
	}
}

func TestCheckKubernetesAcceptsCompleteDeployment(t *testing.T) {
	manifest := "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n        - name: api\n          image: registry.example.com/api:v1.2.3\n          securityContext:\n            runAsNonRoot: true\n            allowPrivilegeEscalation: false\n          resources:\n            requests:\n              cpu: 100m\n              memory: 128Mi\n            limits:\n              cpu: 500m\n              memory: 512Mi\n          readinessProbe:\n            httpGet:\n              path: /ready\n              port: 8080\n          livenessProbe:\n            httpGet:\n              path: /live\n              port: 8080"
	if findings := CheckKubernetes(manifest, "deployment.yaml", true); len(findings) != 0 {
		t.Fatalf("complete deployment should pass deterministic checks: %+v", findings)
	}
}

func TestCheckKubernetesChecksEveryContainer(t *testing.T) {
	manifest := "apiVersion: apps/v1\nkind: Deployment\nspec:\n  replicas: 2\n  template:\n    spec:\n      containers:\n        - name: api\n          image: api:v1\n          resources:\n            requests: {cpu: 100m, memory: 128Mi}\n            limits: {cpu: 500m, memory: 512Mi}\n          readinessProbe: {httpGet: {path: /ready, port: 8080}}\n          livenessProbe: {httpGet: {path: /live, port: 8080}}\n        - name: sidecar\n          image: sidecar:v1"
	findings := CheckKubernetes(manifest, "deployment.yaml", true)
	var resourceSidecar, probeSidecar bool
	for _, finding := range findings {
		if finding.Code == "K8S_RESOURCE_LIMITS" && strings.Contains(finding.Evidence, "sidecar") {
			resourceSidecar = true
		}
		if finding.Code == "K8S_HEALTH_PROBES" && strings.Contains(finding.Evidence, "sidecar") {
			probeSidecar = true
		}
	}
	if !resourceSidecar || !probeSidecar {
		t.Fatalf("every container must be checked independently: %+v", findings)
	}
}

func TestCheckConfigScopesNestedAuthEnabledFlag(t *testing.T) {
	unsafe := CheckConfig("auth:\n  enabled: false\nfeature:\n  enabled: false", "application.yaml", true)
	if !detectionCodeSet(unsafe)["CONFIG_AUTH_DISABLED"] {
		t.Fatalf("nested auth.enabled=false must be detected: %+v", unsafe)
	}
	safe := CheckConfig("auth:\n  enabled: true\nfeature:\n  enabled: false", "application.yaml", true)
	if detectionCodeSet(safe)["CONFIG_AUTH_DISABLED"] {
		t.Fatalf("unrelated enabled=false must not be attributed to auth: %+v", safe)
	}
}
