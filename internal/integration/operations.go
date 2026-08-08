package integration

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

const operationsEventMaxAge = 400 * 24 * time.Hour

type OperationsPayload struct {
	EventID                string                       `json:"event_id"`
	Source                 string                       `json:"source"`
	Kind                   model.OutcomeSignalKind      `json:"kind"`
	Status                 string                       `json:"status"`
	ChangeID               string                       `json:"change_id"`
	IncidentID             string                       `json:"incident_id,omitempty"`
	OperationID            string                       `json:"operation_id,omitempty"`
	Severity               string                       `json:"severity,omitempty"`
	MetricName             string                       `json:"metric_name,omitempty"`
	MetricUnit             string                       `json:"metric_unit,omitempty"`
	MetricDirection        model.OutcomeMetricDirection `json:"metric_direction,omitempty"`
	BaselineValue          *float64                     `json:"baseline_value,omitempty"`
	ObservedValue          *float64                     `json:"observed_value,omitempty"`
	ObjectiveValue         *float64                     `json:"objective_value,omitempty"`
	Tolerance              *float64                     `json:"tolerance,omitempty"`
	BaselineWindowStart    *time.Time                   `json:"baseline_window_start,omitempty"`
	BaselineWindowEnd      *time.Time                   `json:"baseline_window_end,omitempty"`
	ObservationWindowStart *time.Time                   `json:"observation_window_start,omitempty"`
	ObservationWindowEnd   *time.Time                   `json:"observation_window_end,omitempty"`
	ExternalURL            string                       `json:"external_url,omitempty"`
	Detail                 string                       `json:"detail,omitempty"`
	OccurredAt             time.Time                    `json:"occurred_at"`
}

func VerifyOperations(config Config, headers http.Header) error {
	if config.OperationsToken == "" {
		return ErrNotConfigured
	}
	provided := bearerToken(headers.Get("Authorization"))
	if provided == "" || subtle.ConstantTimeCompare([]byte(provided), []byte(config.OperationsToken)) != 1 {
		return ErrUnauthorized
	}
	return nil
}

func ParseOperations(config Config, body []byte, now time.Time) (model.OutcomeSignal, error) {
	var payload OperationsPayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return model.OutcomeSignal{}, ErrInvalidPayload
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return model.OutcomeSignal{}, ErrInvalidPayload
	}

	payload.EventID = clean(payload.EventID, 160)
	payload.Source = strings.ToUpper(clean(payload.Source, 80))
	payload.Status = strings.ToUpper(clean(payload.Status, 48))
	payload.ChangeID = clean(payload.ChangeID, 128)
	payload.IncidentID = clean(payload.IncidentID, 160)
	payload.OperationID = clean(payload.OperationID, 160)
	payload.Severity = strings.ToUpper(clean(payload.Severity, 32))
	payload.MetricName = clean(payload.MetricName, 160)
	payload.MetricUnit = clean(payload.MetricUnit, 40)
	payload.ExternalURL = safeExternalURL(payload.ExternalURL)
	payload.Detail = clean(payload.Detail, 1000)
	if payload.EventID == "" || payload.Source == "" || payload.ChangeID == "" || payload.OccurredAt.IsZero() {
		return model.OutcomeSignal{}, ErrInvalidPayload
	}
	payload.OccurredAt = payload.OccurredAt.UTC()
	now = now.UTC()
	if payload.OccurredAt.After(now.Add(5*time.Minute)) || payload.OccurredAt.Before(now.Add(-operationsEventMaxAge)) {
		return model.OutcomeSignal{}, ErrReplay
	}

	switch payload.Kind {
	case model.OutcomeSignalIncident:
		if payload.IncidentID == "" || !oneOf(payload.Status, "OPEN", "TRIGGERED", "ACKNOWLEDGED", "RESOLVED", "CLOSED") {
			return model.OutcomeSignal{}, ErrInvalidPayload
		}
	case model.OutcomeSignalRollback:
		if payload.OperationID == "" || !oneOf(payload.Status, "STARTED", "SUCCEEDED", "FAILED", "CANCELED", "CANCELLED") {
			return model.OutcomeSignal{}, ErrInvalidPayload
		}
	case model.OutcomeSignalBusinessSLI:
		if payload.Status == "" {
			payload.Status = "OBSERVED"
		}
		if payload.Status != "OBSERVED" || payload.MetricName == "" || payload.MetricUnit == "" ||
			(payload.MetricDirection != model.MetricHigherIsBetter && payload.MetricDirection != model.MetricLowerIsBetter) ||
			!finite(payload.BaselineValue) || !finite(payload.ObservedValue) || !optionalFinite(payload.ObjectiveValue) ||
			!optionalNonNegativeFinite(payload.Tolerance) ||
			!validWindow(payload.BaselineWindowStart, payload.BaselineWindowEnd, payload.OccurredAt) ||
			!validWindow(payload.ObservationWindowStart, payload.ObservationWindowEnd, payload.OccurredAt) ||
			payload.BaselineWindowEnd.After(*payload.ObservationWindowStart) {
			return model.OutcomeSignal{}, ErrInvalidPayload
		}
	default:
		return model.OutcomeSignal{}, ErrUnsupported
	}

	receivedAt := now
	return model.OutcomeSignal{
		OrganizationID: config.OperationsOrganization,
		ExternalID:     payload.EventID, Source: payload.Source, Kind: payload.Kind, Status: payload.Status,
		ChangeID: payload.ChangeID, IncidentID: payload.IncidentID, OperationID: payload.OperationID,
		Severity: payload.Severity, MetricName: payload.MetricName, MetricUnit: payload.MetricUnit,
		MetricDirection: payload.MetricDirection, BaselineValue: payload.BaselineValue,
		ObservedValue: payload.ObservedValue, ObjectiveValue: payload.ObjectiveValue, Tolerance: payload.Tolerance,
		BaselineWindowStart: utcTime(payload.BaselineWindowStart), BaselineWindowEnd: utcTime(payload.BaselineWindowEnd),
		ObservationWindowStart: utcTime(payload.ObservationWindowStart), ObservationWindowEnd: utcTime(payload.ObservationWindowEnd),
		ExternalURL: payload.ExternalURL, Detail: payload.Detail, OccurredAt: payload.OccurredAt, ReceivedAt: receivedAt,
	}, nil
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func finite(value *float64) bool {
	return value != nil && !math.IsNaN(*value) && !math.IsInf(*value, 0)
}

func optionalFinite(value *float64) bool {
	return value == nil || finite(value)
}

func optionalNonNegativeFinite(value *float64) bool {
	return value == nil || (finite(value) && *value >= 0)
}

func validWindow(start, end *time.Time, occurredAt time.Time) bool {
	if start == nil || end == nil {
		return false
	}
	startUTC, endUTC := start.UTC(), end.UTC()
	return startUTC.Before(endUTC) && !endUTC.After(occurredAt.Add(5*time.Minute))
}

func utcTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	converted := value.UTC()
	return &converted
}
