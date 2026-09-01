package service

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

const (
	evidenceToolFindings   = "get_rule_findings"
	evidenceToolExperiment = "get_experiment_report"
	evidenceToolPassports  = "get_change_passports"
)

// EvidenceQuery contains the already-authorized scope and explicit query input
// for one Evidence Navigator tool invocation.
type EvidenceQuery struct {
	Change         model.ChangeRequest
	OrganizationID string
	Input          string
}

// EvidenceQueryResult is produced by the same execution that reads evidence.
// Data is consumed by answer composition; Trace is persisted for auditability.
type EvidenceQueryResult struct {
	Data  any
	Err   error
	Trace model.AgentToolTrace
}

// EvidenceQueryTool is an explicitly read-only Evidence Navigator query. The
// interface intentionally has no mutation, approval, passport issuance, gate
// consumption, deployment, or publication capability.
type EvidenceQueryTool interface {
	Name() string
	Execute(ctx context.Context, query EvidenceQuery) (any, string, error)
}

// EvidenceQueryRegistry is a closed registry used only by Evidence Navigator.
// Tests can inject counting and failing tools without widening production tools.
type EvidenceQueryRegistry struct {
	mu    sync.RWMutex
	tools map[string]EvidenceQueryTool
	now   func() time.Time
}

func NewEvidenceQueryRegistry(tools ...EvidenceQueryTool) *EvidenceQueryRegistry {
	registry := &EvidenceQueryRegistry{tools: make(map[string]EvidenceQueryTool), now: time.Now}
	for _, tool := range tools {
		registry.Register(tool)
	}
	return registry
}

func (r *EvidenceQueryRegistry) Register(tool EvidenceQueryTool) {
	if r == nil || tool == nil || tool.Name() == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Execute atomically invokes a registered tool and derives data, error,
// duration, and trace from that invocation. A failed invocation still returns a
// trace, while Data is nil and must not be used to construct citations.
func (r *EvidenceQueryRegistry) Execute(ctx context.Context, name string, query EvidenceQuery) EvidenceQueryResult {
	started := time.Now()
	if r != nil && r.now != nil {
		started = r.now()
	}
	trace := model.AgentToolTrace{Tool: name, Input: query.Input}

	if r == nil {
		err := fmt.Errorf("evidence query registry unavailable")
		trace.Error = err.Error()
		trace.Duration = time.Since(started).String()
		return EvidenceQueryResult{Err: err, Trace: trace}
	}
	r.mu.RLock()
	tool, ok := r.tools[name]
	r.mu.RUnlock()
	if !ok {
		err := fmt.Errorf("read-only evidence tool not registered")
		trace.Error = err.Error()
		trace.Duration = r.elapsed(started)
		return EvidenceQueryResult{Err: err, Trace: trace}
	}

	data, output, err := tool.Execute(ctx, query)
	trace.Duration = r.elapsed(started)
	if err != nil {
		trace.Error = err.Error()
		return EvidenceQueryResult{Err: err, Trace: trace}
	}
	trace.Output = output
	return EvidenceQueryResult{Data: data, Trace: trace}
}

func (r *EvidenceQueryRegistry) elapsed(started time.Time) string {
	finished := time.Now()
	if r != nil && r.now != nil {
		finished = r.now()
	}
	duration := finished.Sub(started)
	if duration < 0 {
		duration = 0
	}
	return duration.String()
}

type findingEvidence struct {
	Blocking []model.Finding
	Open     []model.Finding
}

type findingEvidenceTool struct{}

func (findingEvidenceTool) Name() string { return evidenceToolFindings }
func (findingEvidenceTool) Execute(_ context.Context, query EvidenceQuery) (any, string, error) {
	blockingOnly := query.Input == "filter=unresolved_blocking"
	blocking := make([]model.Finding, 0)
	open := make([]model.Finding, 0)
	for _, finding := range query.Change.Findings {
		if finding.Status != model.FindingOpen && finding.Status != model.FindingAssigned {
			continue
		}
		if finding.Blocking {
			blocking = append(blocking, finding)
		} else if !blockingOnly {
			open = append(open, finding)
		}
	}
	data := findingEvidence{Blocking: blocking, Open: open}
	return data, fmt.Sprintf("%d blocking, %d non_blocking", len(blocking), len(open)), nil
}

type experimentEvidenceTool struct{}

func (experimentEvidenceTool) Name() string { return evidenceToolExperiment }
func (experimentEvidenceTool) Execute(_ context.Context, query EvidenceQuery) (any, string, error) {
	if query.Change.Experiment == nil {
		return (*model.ExperimentReport)(nil), "NOT_RUN / no report", nil
	}
	experiment := *query.Change.Experiment
	return &experiment, fmt.Sprintf("%s / %s", experiment.Mode, experiment.Status), nil
}

type passportEvidenceTool struct{ store *store.Store }

func (passportEvidenceTool) Name() string { return evidenceToolPassports }
func (tool passportEvidenceTool) Execute(_ context.Context, query EvidenceQuery) (any, string, error) {
	if tool.store == nil {
		return nil, "", fmt.Errorf("passport evidence store unavailable")
	}
	passports := tool.store.PassportsByChange(query.OrganizationID, query.Change.ID)
	return passports, fmt.Sprintf("%d passports", len(passports)), nil
}

func defaultEvidenceQueryRegistry(data *store.Store) *EvidenceQueryRegistry {
	return NewEvidenceQueryRegistry(findingEvidenceTool{}, experimentEvidenceTool{}, passportEvidenceTool{store: data})
}
