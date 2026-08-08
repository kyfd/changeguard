package agentgateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const metricsStateSchema = "changeguard-agent-metrics/v1"

type persistedMetricsState struct {
	Schema              string                      `json:"schema"`
	WindowStartedAt     time.Time                   `json:"window_started_at"`
	WindowSeconds       int64                       `json:"window_seconds"`
	UpdatedAt           time.Time                   `json:"updated_at"`
	Total               uint64                      `json:"total"`
	Successful          uint64                      `json:"successful"`
	Failed              uint64                      `json:"failed"`
	Rejected            uint64                      `json:"rejected"`
	UpstreamErrors      uint64                      `json:"upstream_errors"`
	Injections          uint64                      `json:"injections"`
	DurationTotalMS     int64                       `json:"duration_total_ms"`
	DurationsMS         []int64                     `json:"durations_ms"`
	EligibleDurationsMS []int64                     `json:"eligible_durations_ms,omitempty"`
	Operations          map[string]OperationMetrics `json:"operations"`
	HMACSHA256          string                      `json:"hmac_sha256"`
}

func openGatewayMetrics(path string, key []byte, window time.Duration) (*gatewayMetrics, error) {
	return openGatewayMetricsWithClock(path, key, window, time.Now)
}

func openGatewayMetricsWithClock(path string, key []byte, window time.Duration, now func() time.Time) (*gatewayMetrics, error) {
	if len(key) < 32 {
		return nil, errors.New("metrics state key must contain at least 32 bytes")
	}
	if now == nil {
		now = time.Now
	}
	m := &gatewayMetrics{
		path: path, key: append([]byte(nil), key...), window: window, now: now,
		verified: true, operations: make(map[string]OperationMetrics), durations: make([]int64, 0, 512),
		eligibleDurations: make([]int64, 0, 512),
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create metrics state directory: %w", err)
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		m.windowStarted = m.currentTime()
		m.dirty = true
		if err := m.persistLocked(m.windowStarted); err != nil {
			return nil, fmt.Errorf("initialize metrics state: %w", err)
		}
		return m, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read metrics state: %w", err)
	}
	if len(data) == 0 || len(data) > 1<<20 {
		return nil, errors.New("metrics state must be between 1 byte and 1 MiB")
	}
	state, err := m.decodeAndVerify(data)
	if err != nil {
		return nil, err
	}
	m.loadLocked(state)
	current := m.currentTime()
	if state.WindowSeconds != int64(window.Seconds()) || !current.Before(m.windowStarted.Add(window)) {
		m.resetLocked(current)
		m.dirty = true
		if err := m.persistLocked(current); err != nil {
			return nil, fmt.Errorf("rotate metrics state: %w", err)
		}
	}
	return m, nil
}

func (m *gatewayMetrics) currentTime() time.Time {
	if m.now == nil {
		return time.Now().UTC()
	}
	return m.now().UTC()
}

func (m *gatewayMetrics) State() MetricsPersistenceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path != "" && m.verified {
		if err := m.verifyDiskLocked(); err != nil {
			m.verified = false
		} else if err := m.rotateIfExpiredLocked(m.currentTime()); err != nil {
			m.verified = false
		}
	}
	return MetricsPersistenceState{
		Verified: m.verified && !m.dirty, WindowStartedAt: m.windowStarted,
		WindowEndsAt: m.windowStarted.Add(m.window), LastPersistedAt: m.lastPersisted,
		WindowSeconds: int64(m.window.Seconds()),
	}
}

func (m *gatewayMetrics) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.path == "" || (!m.dirty && m.verified) {
		return nil
	}
	if !m.verified {
		return errors.New("metrics state is not verified")
	}
	return m.persistLocked(m.currentTime())
}

func (m *gatewayMetrics) rotateIfExpiredLocked(now time.Time) error {
	if m.window <= 0 || now.Before(m.windowStarted.Add(m.window)) {
		return nil
	}
	m.resetLocked(now)
	if m.path == "" {
		return nil
	}
	m.dirty = true
	return m.persistLocked(now)
}

func (m *gatewayMetrics) resetLocked(now time.Time) {
	m.windowStarted = now.UTC()
	m.total = 0
	m.successful = 0
	m.failed = 0
	m.rejected = 0
	m.upstreamErrors = 0
	m.injections = 0
	m.durationTotal = 0
	m.durations = make([]int64, 0, 512)
	m.eligibleDurations = make([]int64, 0, 512)
	m.operations = make(map[string]OperationMetrics)
}

func (m *gatewayMetrics) persistLocked(now time.Time) error {
	state := persistedMetricsState{
		Schema: metricsStateSchema, WindowStartedAt: m.windowStarted.UTC(),
		WindowSeconds: int64(m.window.Seconds()), UpdatedAt: now.UTC(),
		Total: m.total, Successful: m.successful, Failed: m.failed, Rejected: m.rejected,
		UpstreamErrors: m.upstreamErrors, Injections: m.injections,
		DurationTotalMS: m.durationTotal, DurationsMS: append([]int64(nil), m.durations...),
		EligibleDurationsMS: append([]int64(nil), m.eligibleDurations...),
		Operations:          cloneOperationMetrics(m.operations),
	}
	signature, err := m.signState(state)
	if err != nil {
		m.verified = false
		return fmt.Errorf("sign metrics state: %w", err)
	}
	state.HMACSHA256 = signature
	encoded, err := json.Marshal(state)
	if err != nil {
		m.verified = false
		return fmt.Errorf("encode metrics state: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeAtomicFile(m.path, encoded, 0o640); err != nil {
		m.verified = false
		return fmt.Errorf("persist metrics state: %w", err)
	}
	m.lastPersisted = state.UpdatedAt
	m.lastHMAC = signature
	m.dirty = false
	m.verified = true
	return nil
}

func (m *gatewayMetrics) verifyDiskLocked() error {
	if m.path == "" {
		return nil
	}
	if m.dirty {
		return errors.New("metrics state has unpersisted changes")
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		return fmt.Errorf("read metrics state: %w", err)
	}
	state, err := m.decodeAndVerify(data)
	if err != nil {
		return err
	}
	if m.lastHMAC != "" && !hmac.Equal([]byte(state.HMACSHA256), []byte(m.lastHMAC)) {
		return errors.New("metrics state changed outside the gateway process")
	}
	return nil
}

func (m *gatewayMetrics) decodeAndVerify(data []byte) (persistedMetricsState, error) {
	var state persistedMetricsState
	if err := json.Unmarshal(data, &state); err != nil {
		return persistedMetricsState{}, fmt.Errorf("decode metrics state: %w", err)
	}
	if state.Schema != metricsStateSchema {
		return persistedMetricsState{}, fmt.Errorf("unsupported metrics state schema %q", state.Schema)
	}
	if state.WindowStartedAt.IsZero() || state.UpdatedAt.Before(state.WindowStartedAt) {
		return persistedMetricsState{}, errors.New("metrics state has invalid timestamps")
	}
	if state.WindowSeconds < int64(time.Hour.Seconds()) || state.WindowSeconds > int64((31*24*time.Hour).Seconds()) {
		return persistedMetricsState{}, errors.New("metrics state has an invalid window")
	}
	if state.Total != state.Successful+state.Failed+state.Rejected {
		return persistedMetricsState{}, errors.New("metrics state counters are inconsistent")
	}
	if len(state.DurationsMS) > 512 {
		return persistedMetricsState{}, errors.New("metrics state contains too many duration samples")
	}
	if len(state.EligibleDurationsMS) > 512 {
		return persistedMetricsState{}, errors.New("metrics state contains too many eligible duration samples")
	}
	var operationTotal, operationSuccessful, operationFailed, operationRejected uint64
	for operation, metric := range state.Operations {
		if operation == "" || metric.Total != metric.Successful+metric.Failed+metric.Rejected {
			return persistedMetricsState{}, errors.New("metrics state operation counters are inconsistent")
		}
		operationTotal += metric.Total
		operationSuccessful += metric.Successful
		operationFailed += metric.Failed
		operationRejected += metric.Rejected
	}
	if operationTotal != state.Total || operationSuccessful != state.Successful || operationFailed != state.Failed || operationRejected != state.Rejected {
		return persistedMetricsState{}, errors.New("metrics state operation totals do not match global totals")
	}
	var sampledDuration int64
	for _, duration := range state.DurationsMS {
		if duration < 0 {
			return persistedMetricsState{}, errors.New("metrics state contains a negative duration")
		}
		sampledDuration += duration
	}
	for _, duration := range state.EligibleDurationsMS {
		if duration < 0 {
			return persistedMetricsState{}, errors.New("metrics state contains a negative eligible duration")
		}
	}
	if state.DurationTotalMS < sampledDuration {
		return persistedMetricsState{}, errors.New("metrics state duration total is inconsistent")
	}
	expected, err := m.signState(state)
	if err != nil {
		return persistedMetricsState{}, err
	}
	if state.HMACSHA256 == "" || !hmac.Equal([]byte(state.HMACSHA256), []byte(expected)) {
		return persistedMetricsState{}, errors.New("metrics state failed HMAC verification")
	}
	return state, nil
}

func (m *gatewayMetrics) signState(state persistedMetricsState) (string, error) {
	state.HMACSHA256 = ""
	encoded, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte(metricsStateSchema))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func (m *gatewayMetrics) loadLocked(state persistedMetricsState) {
	m.windowStarted = state.WindowStartedAt.UTC()
	m.lastPersisted = state.UpdatedAt.UTC()
	m.lastHMAC = state.HMACSHA256
	m.verified = true
	m.dirty = false
	m.total = state.Total
	m.successful = state.Successful
	m.failed = state.Failed
	m.rejected = state.Rejected
	m.upstreamErrors = state.UpstreamErrors
	m.injections = state.Injections
	m.durationTotal = state.DurationTotalMS
	m.durations = append(make([]int64, 0, 512), state.DurationsMS...)
	m.eligibleDurations = append(make([]int64, 0, 512), state.EligibleDurationsMS...)
	m.operations = cloneOperationMetrics(state.Operations)
}

func cloneOperationMetrics(source map[string]OperationMetrics) map[string]OperationMetrics {
	clone := make(map[string]OperationMetrics, len(source))
	for operation, metric := range source {
		clone[operation] = metric
	}
	return clone
}

func writeAtomicFile(path string, content []byte, mode os.FileMode) (err error) {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if err != nil {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err = temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err = temporary.Write(content); err != nil {
		return err
	}
	if err = temporary.Sync(); err != nil {
		return err
	}
	if err = temporary.Close(); err != nil {
		return err
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
