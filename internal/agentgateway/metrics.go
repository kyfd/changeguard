package agentgateway

import (
	"math"
	"sort"
	"sync"
	"time"
)

type OperationMetrics struct {
	Total      uint64 `json:"total"`
	Successful uint64 `json:"successful"`
	Failed     uint64 `json:"failed"`
	Rejected   uint64 `json:"rejected"`
}

type MetricsSnapshot struct {
	Total                   uint64                      `json:"total"`
	Successful              uint64                      `json:"successful"`
	Failed                  uint64                      `json:"failed"`
	Rejected                uint64                      `json:"rejected"`
	UpstreamErrors          uint64                      `json:"upstream_errors"`
	InjectionSuspectedTotal uint64                      `json:"injection_suspected_total"`
	AverageDurationMS       int64                       `json:"average_duration_ms"`
	P95DurationMS           int64                       `json:"p95_duration_ms"`
	EligibleP95DurationMS   int64                       `json:"eligible_p95_duration_ms"`
	Operations              map[string]OperationMetrics `json:"operations"`
}

type MetricsPersistenceState struct {
	Verified        bool      `json:"verified"`
	WindowStartedAt time.Time `json:"window_started_at"`
	WindowEndsAt    time.Time `json:"window_ends_at"`
	LastPersistedAt time.Time `json:"last_persisted_at"`
	WindowSeconds   int64     `json:"window_seconds"`
}

type SLOSnapshot struct {
	Status                    string    `json:"status"`
	AvailabilityPercent       float64   `json:"availability_percent"`
	AvailabilityTargetPercent float64   `json:"availability_target_percent"`
	EligibleRequests          uint64    `json:"eligible_requests"`
	P95DurationMS             int64     `json:"p95_duration_ms"`
	P95TargetMS               int64     `json:"p95_target_ms"`
	AvailabilityWithinTarget  bool      `json:"availability_within_target"`
	LatencyWithinTarget       bool      `json:"latency_within_target"`
	AuditChainVerified        bool      `json:"audit_chain_verified"`
	MetricsStateVerified      bool      `json:"metrics_state_verified"`
	UpstreamReady             bool      `json:"upstream_ready"`
	WindowStartedAt           time.Time `json:"window_started_at"`
	WindowEndsAt              time.Time `json:"window_ends_at"`
}

func calculateSLO(metrics MetricsSnapshot, metricsState MetricsPersistenceState, audit AuditState, upstreamReady bool, p95Target time.Duration, availabilityTarget float64) SLOSnapshot {
	eligible := metrics.Successful + metrics.Failed
	availability := 100.0
	if eligible > 0 {
		availability = float64(metrics.Successful) * 100 / float64(eligible)
	}
	availability = math.Round(availability*100) / 100
	p95TargetMS := p95Target.Milliseconds()
	availabilityOK := availability >= availabilityTarget
	latencyOK := eligible == 0 || metrics.EligibleP95DurationMS <= p95TargetMS
	status := "healthy"
	if !audit.Verified || !metricsState.Verified || !upstreamReady || !availabilityOK || !latencyOK {
		status = "degraded"
	} else if eligible == 0 {
		status = "observing"
	}
	return SLOSnapshot{
		Status: status, AvailabilityPercent: availability,
		AvailabilityTargetPercent: availabilityTarget, EligibleRequests: eligible,
		P95DurationMS: metrics.EligibleP95DurationMS, P95TargetMS: p95TargetMS,
		AvailabilityWithinTarget: availabilityOK, LatencyWithinTarget: latencyOK,
		AuditChainVerified: audit.Verified, MetricsStateVerified: metricsState.Verified,
		UpstreamReady: upstreamReady, WindowStartedAt: metricsState.WindowStartedAt,
		WindowEndsAt: metricsState.WindowEndsAt,
	}
}

type gatewayMetrics struct {
	mu                sync.RWMutex
	path              string
	key               []byte
	window            time.Duration
	now               func() time.Time
	windowStarted     time.Time
	lastPersisted     time.Time
	lastHMAC          string
	verified          bool
	dirty             bool
	total             uint64
	successful        uint64
	failed            uint64
	rejected          uint64
	upstreamErrors    uint64
	injections        uint64
	durationTotal     int64
	durations         []int64
	eligibleDurations []int64
	operations        map[string]OperationMetrics
}

func newGatewayMetrics() *gatewayMetrics {
	now := time.Now().UTC()
	return &gatewayMetrics{
		window: 24 * time.Hour, now: time.Now, windowStarted: now, verified: true,
		operations: make(map[string]OperationMetrics), durations: make([]int64, 0, 512),
		eligibleDurations: make([]int64, 0, 512),
	}
}

func (m *gatewayMetrics) record(operation, outcome string, duration time.Duration, injection, upstreamError bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.currentTime()
	if m.path != "" {
		if err := m.verifyDiskLocked(); err != nil {
			m.verified = false
			return err
		}
		if err := m.rotateIfExpiredLocked(now); err != nil {
			return err
		}
	}
	m.total++
	operationMetric := m.operations[operation]
	operationMetric.Total++
	switch outcome {
	case "success":
		m.successful++
		operationMetric.Successful++
	case "rejected":
		m.rejected++
		operationMetric.Rejected++
	default:
		m.failed++
		operationMetric.Failed++
	}
	if upstreamError {
		m.upstreamErrors++
	}
	if injection {
		m.injections++
	}
	durationMS := duration.Milliseconds()
	if durationMS < 0 {
		durationMS = 0
	}
	m.durationTotal += durationMS
	if len(m.durations) >= 512 {
		copy(m.durations, m.durations[1:])
		m.durations[len(m.durations)-1] = durationMS
	} else {
		m.durations = append(m.durations, durationMS)
	}
	if outcome != "rejected" {
		if len(m.eligibleDurations) >= 512 {
			copy(m.eligibleDurations, m.eligibleDurations[1:])
			m.eligibleDurations[len(m.eligibleDurations)-1] = durationMS
		} else {
			m.eligibleDurations = append(m.eligibleDurations, durationMS)
		}
	}
	m.operations[operation] = operationMetric
	if m.path == "" {
		return nil
	}
	m.dirty = true
	return m.persistLocked(now)
}

func (m *gatewayMetrics) snapshot() MetricsSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path != "" && m.verified {
		if err := m.verifyDiskLocked(); err != nil {
			m.verified = false
		} else {
			_ = m.rotateIfExpiredLocked(m.currentTime())
		}
	}
	return m.snapshotLocked()
}

func (m *gatewayMetrics) snapshotLocked() MetricsSnapshot {
	operations := make(map[string]OperationMetrics, len(m.operations))
	for key, value := range m.operations {
		operations[key] = value
	}
	durations := append([]int64(nil), m.durations...)
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	eligibleDurations := append([]int64(nil), m.eligibleDurations...)
	sort.Slice(eligibleDurations, func(i, j int) bool { return eligibleDurations[i] < eligibleDurations[j] })
	var average, p95, eligibleP95 int64
	if m.total > 0 {
		average = m.durationTotal / int64(m.total)
	}
	if len(durations) > 0 {
		index := (95*len(durations) + 99) / 100
		if index < 1 {
			index = 1
		}
		p95 = durations[index-1]
	}
	if len(eligibleDurations) > 0 {
		index := (95*len(eligibleDurations) + 99) / 100
		if index < 1 {
			index = 1
		}
		eligibleP95 = eligibleDurations[index-1]
	}
	return MetricsSnapshot{
		Total:                   m.total,
		Successful:              m.successful,
		Failed:                  m.failed,
		Rejected:                m.rejected,
		UpstreamErrors:          m.upstreamErrors,
		InjectionSuspectedTotal: m.injections,
		AverageDurationMS:       average,
		P95DurationMS:           p95,
		EligibleP95DurationMS:   eligibleP95,
		Operations:              operations,
	}
}

type rateBucket struct {
	tokens float64
	last   time.Time
	seen   time.Time
}

type tokenLimiter struct {
	mu       sync.Mutex
	rate     float64
	capacity float64
	buckets  map[string]rateBucket
	lastGC   time.Time
}

func newTokenLimiter(ratePerMinute, burst int) *tokenLimiter {
	return &tokenLimiter{
		rate:     float64(ratePerMinute) / 60.0,
		capacity: float64(burst),
		buckets:  make(map[string]rateBucket),
		lastGC:   time.Now(),
	}
}

func (l *tokenLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastGC) >= 5*time.Minute {
		for bucketKey, bucket := range l.buckets {
			if now.Sub(bucket.seen) > 15*time.Minute {
				delete(l.buckets, bucketKey)
			}
		}
		l.lastGC = now
	}
	bucket, ok := l.buckets[key]
	if !ok {
		bucket = rateBucket{tokens: l.capacity, last: now}
	}
	elapsed := now.Sub(bucket.last).Seconds()
	if elapsed > 0 {
		bucket.tokens += elapsed * l.rate
		if bucket.tokens > l.capacity {
			bucket.tokens = l.capacity
		}
	}
	bucket.last = now
	bucket.seen = now
	allowed := bucket.tokens >= 1
	if allowed {
		bucket.tokens--
	}
	l.buckets[key] = bucket
	return allowed
}
