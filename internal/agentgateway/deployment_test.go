package agentgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnterpriseMonitoringDeploymentArtifacts(t *testing.T) {
	directory := filepath.Join("..", "..", "deploy", "agent-gateway")
	assertFileMarkers(t, filepath.Join(directory, "changeguard-agent-monitor.sh"), []string{
		`/health/ready`, `/health/slo`, `changeguard-agent-monitor/v1`,
		`CHANGEGUARD_AGENT_ALERT_WEBHOOK_URL`, `curl --config`, `previous_status`,
	})
	assertFileMarkers(t, filepath.Join(directory, "changeguard-agent-monitor.service"), []string{
		`User=changeguard-agent`, `ProtectSystem=strict`, `ReadWritePaths=/opt/changeguard-agent/data`,
	})
	assertFileMarkers(t, filepath.Join(directory, "changeguard-agent-monitor.timer"), []string{
		`OnUnitActiveSec=1min`, `Persistent=true`, `WantedBy=timers.target`,
	})
	assertFileMarkers(t, filepath.Join(directory, "prometheus-alerts.yaml"), []string{
		`ChangeGuardAgentGatewayDown`, `ChangeGuardAgentAuditChainUnverified`,
		`ChangeGuardAgentMetricsStateUnverified`, `ChangeGuardAgentAvailabilitySLOBreach`,
		`ChangeGuardAgentLatencySLOBreach`, `ChangeGuardAgentAuditFileLarge`,
	})
	assertFileMarkers(t, filepath.Join(directory, "OPERATIONS.md"), []string{
		`fixed-duration checkpoint`, `Do **not** truncate`, `systemctl enable --now changeguard-agent-monitor.timer`,
	})
	assertFileMarkers(t, filepath.Join("..", "..", "deploy", "production", "changeguard-backup.sh"), []string{
		`changeguard-backup/v2`, `manifest.sha256`, `sha256sum -c`, `audit.jsonl`, `metrics.json`,
		`readlink -f /opt/changeguard-agent/current`, `CHANGEGUARD_BACKUP_RETENTION`, `refusing to prune unexpected path`,
	})
}

func assertFileMarkers(t *testing.T, path string, markers []string) {
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
