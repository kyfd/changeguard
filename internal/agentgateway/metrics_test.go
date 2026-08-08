package agentgateway

import (
	"testing"
	"time"
)

func TestCalculateSLOExcludesSecurityRejectionsFromAvailability(t *testing.T) {
	metrics := MetricsSnapshot{Successful: 9, Failed: 1, Rejected: 50, P95DurationMS: 1200, EligibleP95DurationMS: 1200}
	slo := calculateSLO(metrics, verifiedMetricsState(), AuditState{Verified: true}, true, 30*time.Second, 99)
	if slo.EligibleRequests != 10 || slo.AvailabilityPercent != 90 {
		t.Fatalf("security rejections must not lower availability: %+v", slo)
	}
	if slo.Status != "degraded" || slo.AvailabilityWithinTarget {
		t.Fatalf("availability miss must degrade SLO: %+v", slo)
	}
}

func TestCalculateSLOObservesEmptyWindow(t *testing.T) {
	slo := calculateSLO(MetricsSnapshot{}, verifiedMetricsState(), AuditState{Verified: true}, true, 30*time.Second, 99)
	if slo.Status != "observing" || slo.EligibleRequests != 0 || slo.AvailabilityPercent != 100 {
		t.Fatalf("empty runtime window should be observing: %+v", slo)
	}
}

func TestCalculateSLODegradesForLatencyOrDependencyHealth(t *testing.T) {
	metrics := MetricsSnapshot{Successful: 10, P95DurationMS: 31001, EligibleP95DurationMS: 31001}
	slo := calculateSLO(metrics, verifiedMetricsState(), AuditState{Verified: true}, true, 30*time.Second, 99)
	if slo.Status != "degraded" || slo.LatencyWithinTarget {
		t.Fatalf("latency miss must degrade SLO: %+v", slo)
	}
	slo = calculateSLO(MetricsSnapshot{Successful: 1}, verifiedMetricsState(), AuditState{Verified: false}, true, 30*time.Second, 99)
	if slo.Status != "degraded" || slo.AuditChainVerified {
		t.Fatalf("audit verification failure must degrade SLO: %+v", slo)
	}
}

func TestCalculateSLOExcludesRejectedLatencySamples(t *testing.T) {
	metrics := newGatewayMetrics()
	if err := metrics.record("agent-ask", "success", time.Second, false, false); err != nil {
		t.Fatal(err)
	}
	if err := metrics.record("agent-ask", "rejected", 90*time.Second, false, false); err != nil {
		t.Fatal(err)
	}
	snapshot := metrics.snapshot()
	if snapshot.P95DurationMS != 90000 || snapshot.EligibleP95DurationMS != 1000 {
		t.Fatalf("latency samples were not separated: %+v", snapshot)
	}
	slo := calculateSLO(snapshot, verifiedMetricsState(), AuditState{Verified: true}, true, 30*time.Second, 99)
	if slo.Status != "healthy" || slo.P95DurationMS != 1000 {
		t.Fatalf("rejected latency must not breach the service SLO: %+v", slo)
	}
}

func TestCalculateSLODegradesWhenMetricsStateIsUnverified(t *testing.T) {
	state := verifiedMetricsState()
	state.Verified = false
	slo := calculateSLO(MetricsSnapshot{Successful: 1}, state, AuditState{Verified: true}, true, 30*time.Second, 99)
	if slo.Status != "degraded" || slo.MetricsStateVerified {
		t.Fatalf("metrics integrity failure must degrade SLO: %+v", slo)
	}
}

func verifiedMetricsState() MetricsPersistenceState {
	started := time.Now().UTC().Add(-time.Hour)
	return MetricsPersistenceState{
		Verified: true, WindowStartedAt: started, WindowEndsAt: started.Add(24 * time.Hour),
		LastPersistedAt: time.Now().UTC(), WindowSeconds: int64((24 * time.Hour).Seconds()),
	}
}
