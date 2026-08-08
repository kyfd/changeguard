package agentgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGatewayMetricsPersistAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	key := []byte(strings.Repeat("p", 32))
	current := time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC)
	clock := func() time.Time { return current }
	metrics, err := openGatewayMetricsWithClock(path, key, 24*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.record("agent-ask", "success", 1500*time.Millisecond, true, false); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := metrics.record("agent-ask", "failed", 2500*time.Millisecond, false, true); err != nil {
		t.Fatal(err)
	}
	current = current.Add(time.Minute)
	if err := metrics.record("submit-check", "rejected", 10*time.Millisecond, false, false); err != nil {
		t.Fatal(err)
	}
	before := metrics.State()
	if !before.Verified || before.LastPersistedAt != current {
		t.Fatalf("unexpected persisted state: %+v", before)
	}
	if err := metrics.Close(); err != nil {
		t.Fatal(err)
	}

	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(key)) || !strings.Contains(string(encoded), `"hmac_sha256"`) {
		t.Fatalf("metrics state must be signed without exposing the signing key: %s", encoded)
	}

	reopened, err := openGatewayMetricsWithClock(path, key, 24*time.Hour, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot := reopened.snapshot()
	if snapshot.Total != 3 || snapshot.Successful != 1 || snapshot.Failed != 1 || snapshot.Rejected != 1 {
		t.Fatalf("metrics did not survive restart: %+v", snapshot)
	}
	if snapshot.UpstreamErrors != 1 || snapshot.InjectionSuspectedTotal != 1 || snapshot.P95DurationMS != 2500 || snapshot.EligibleP95DurationMS != 2500 {
		t.Fatalf("operational metadata did not survive restart: %+v", snapshot)
	}
	if snapshot.Operations["agent-ask"].Total != 2 || snapshot.Operations["submit-check"].Rejected != 1 {
		t.Fatalf("operation metrics did not survive restart: %+v", snapshot.Operations)
	}
	after := reopened.State()
	if !after.Verified || after.WindowStartedAt != before.WindowStartedAt || after.LastPersistedAt != before.LastPersistedAt {
		t.Fatalf("window continuity was not preserved: before=%+v after=%+v", before, after)
	}
}

func TestGatewayMetricsRejectTamperingAndFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	key := []byte(strings.Repeat("t", 32))
	metrics, err := openGatewayMetrics(path, key, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.record("agent-ask", "success", time.Second, false, false); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content = []byte(strings.Replace(string(content), `"successful":1`, `"successful":0`, 1))
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatal(err)
	}
	if state := metrics.State(); state.Verified {
		t.Fatalf("post-open tampering must degrade metrics state: %+v", state)
	}
	if err := metrics.record("agent-ask", "success", time.Second, false, false); err == nil {
		t.Fatal("unverified metrics state must reject new checkpoints")
	}
	_ = metrics.Close()
	if _, err := openGatewayMetrics(path, key, 24*time.Hour); err == nil {
		t.Fatal("tampered metrics state must not reopen")
	}
}

func TestGatewayMetricsRotateExpiredFixedWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.json")
	key := []byte(strings.Repeat("w", 32))
	current := time.Date(2026, 8, 8, 8, 0, 0, 0, time.UTC)
	metrics, err := openGatewayMetricsWithClock(path, key, time.Hour, func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	if err := metrics.record("agent-ask", "success", time.Second, false, false); err != nil {
		t.Fatal(err)
	}
	current = current.Add(61 * time.Minute)
	snapshot := metrics.snapshot()
	state := metrics.State()
	if snapshot.Total != 0 || !state.Verified || state.WindowStartedAt != current {
		t.Fatalf("expired fixed window was not rotated: snapshot=%+v state=%+v", snapshot, state)
	}
	if err := metrics.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openGatewayMetricsWithClock(path, key, time.Hour, func() time.Time { return current })
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.snapshot().Total != 0 || reopened.State().WindowStartedAt != current {
		t.Fatal("rotated window did not persist")
	}
}
