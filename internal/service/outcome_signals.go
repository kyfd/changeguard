package service

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

func (s *Service) OutcomeSignalsFor(actorID string, limit int) ([]model.OutcomeSignal, error) {
	actor, err := s.activeActor(actorID)
	if err != nil {
		return nil, err
	}
	changes, err := s.ChangesFor(actorID)
	if err != nil {
		return nil, err
	}
	allowed := make(map[string]bool, len(changes))
	for _, change := range changes {
		allowed[change.ID] = true
	}
	all := s.store.OutcomeSignals(actor.OrganizationID, 0)
	visible := make([]model.OutcomeSignal, 0, len(all))
	for _, signal := range all {
		if allowed[signal.ChangeID] {
			visible = append(visible, signal)
			if limit > 0 && len(visible) == limit {
				break
			}
		}
	}
	return visible, nil
}

func (s *Service) RecordOutcomeSignal(signal model.OutcomeSignal) (model.OutcomeSignal, bool, error) {
	signal.OrganizationID = strings.TrimSpace(signal.OrganizationID)
	signal.ExternalID = strings.TrimSpace(signal.ExternalID)
	signal.Source = strings.ToUpper(strings.TrimSpace(signal.Source))
	signal.Status = strings.ToUpper(strings.TrimSpace(signal.Status))
	signal.ChangeID = strings.TrimSpace(signal.ChangeID)
	signal.IncidentID = strings.TrimSpace(signal.IncidentID)
	signal.OperationID = strings.TrimSpace(signal.OperationID)
	signal.Severity = strings.ToUpper(strings.TrimSpace(signal.Severity))
	signal.MetricName = strings.TrimSpace(signal.MetricName)
	signal.MetricUnit = strings.TrimSpace(signal.MetricUnit)
	signal.Detail = changegate.Redact(strings.TrimSpace(signal.Detail))
	if signal.ID == "" {
		signal.ID = store.NewID("outcome_")
	}
	if signal.ReceivedAt.IsZero() {
		signal.ReceivedAt = time.Now().UTC()
	} else {
		signal.ReceivedAt = signal.ReceivedAt.UTC()
	}
	signal.OccurredAt = signal.OccurredAt.UTC()
	if !validOutcomeSignal(signal) {
		return model.OutcomeSignal{}, false, ErrValidation
	}
	change, err := s.store.Change(signal.ChangeID)
	if err != nil || change.OrganizationID != signal.OrganizationID {
		return model.OutcomeSignal{}, false, ErrValidation
	}

	action, subject := "OUTCOME_SIGNAL_RECORDED", "运维结果"
	switch signal.Kind {
	case model.OutcomeSignalIncident:
		action, subject = "INCIDENT_LINKED", "事故 "+signal.IncidentID
	case model.OutcomeSignalRollback:
		action, subject = "ROLLBACK_EXECUTION_RECORDED", "回滚 "+signal.OperationID
	case model.OutcomeSignalBusinessSLI:
		action, subject = "BUSINESS_SLI_RECORDED", "业务指标 "+signal.MetricName
	}
	detail := fmt.Sprintf("%s 状态 %s，来源 %s", subject, signal.Status, signal.Source)
	if signal.Detail != "" {
		detail += "；" + signal.Detail
	}
	audit := model.AuditEvent{
		OrganizationID: signal.OrganizationID,
		ID:             store.NewID("aud_"),
		ChangeID:       signal.ChangeID,
		ActorID:        "integration_operations",
		ActorName:      signal.Source,
		Action:         action,
		Detail:         detail,
		CreatedAt:      signal.ReceivedAt,
	}
	return s.store.RecordOutcomeSignal(signal, audit)
}

func validOutcomeSignal(signal model.OutcomeSignal) bool {
	if signal.OrganizationID == "" || signal.ExternalID == "" || signal.Source == "" || signal.ChangeID == "" ||
		signal.OccurredAt.IsZero() || signal.ReceivedAt.IsZero() ||
		!validRuneLength(signal.ExternalID, 1, 160) || !validRuneLength(signal.Source, 1, 80) ||
		!validRuneLength(signal.ChangeID, 1, 128) || !validRuneLength(signal.Detail, 0, 1000) {
		return false
	}
	switch signal.Kind {
	case model.OutcomeSignalIncident:
		return validRuneLength(signal.IncidentID, 1, 160) && oneOfOutcomeStatus(signal.Status, "OPEN", "TRIGGERED", "ACKNOWLEDGED", "RESOLVED", "CLOSED")
	case model.OutcomeSignalRollback:
		return validRuneLength(signal.OperationID, 1, 160) && oneOfOutcomeStatus(signal.Status, "STARTED", "SUCCEEDED", "FAILED", "CANCELED", "CANCELLED")
	case model.OutcomeSignalBusinessSLI:
		return signal.Status == "OBSERVED" && validRuneLength(signal.MetricName, 1, 160) && validRuneLength(signal.MetricUnit, 1, 40) &&
			(signal.MetricDirection == model.MetricHigherIsBetter || signal.MetricDirection == model.MetricLowerIsBetter) &&
			finiteOutcomeValue(signal.BaselineValue, true) && finiteOutcomeValue(signal.ObservedValue, true) &&
			finiteOutcomeValue(signal.ObjectiveValue, false) && nonNegativeOutcomeValue(signal.Tolerance) &&
			validOutcomeWindow(signal.BaselineWindowStart, signal.BaselineWindowEnd, signal.OccurredAt) &&
			validOutcomeWindow(signal.ObservationWindowStart, signal.ObservationWindowEnd, signal.OccurredAt) &&
			!signal.BaselineWindowEnd.After(*signal.ObservationWindowStart)
	default:
		return false
	}
}

func oneOfOutcomeStatus(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func finiteOutcomeValue(value *float64, required bool) bool {
	if value == nil {
		return !required
	}
	return !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func nonNegativeOutcomeValue(value *float64) bool {
	return value == nil || (finiteOutcomeValue(value, true) && *value >= 0)
}

func validOutcomeWindow(start, end *time.Time, occurredAt time.Time) bool {
	if start == nil || end == nil {
		return false
	}
	return start.Before(*end) && !end.After(occurredAt.Add(5*time.Minute))
}
