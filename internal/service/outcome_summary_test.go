package service

import (
	"reflect"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestOutcomeSignalStatusFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		kind         model.OutcomeSignalKind
		status, want string
	}{
		{model.OutcomeSignalIncident, "OPEN", "BLOCK"},
		{model.OutcomeSignalIncident, "ACKNOWLEDGED", "BLOCK"},
		{model.OutcomeSignalIncident, "RESOLVED", "PASS"},
		{model.OutcomeSignalRollback, "STARTED", "WARN"},
		{model.OutcomeSignalRollback, "FAILED", "BLOCK"},
		{model.OutcomeSignalRollback, "SUCCEEDED", "PASS"},
		{model.OutcomeSignalRollback, "CANCELED", "WARN"},
		{model.OutcomeSignalBusinessSLI, "BREACH", "WARN"},
		{model.OutcomeSignalBusinessSLI, "OBSERVED", "WARN"},
		{model.OutcomeSignalIncident, "UNKNOWN", "WARN"},
		{"UNKNOWN", "PASS", "WARN"},
	} {
		t.Run(string(tc.kind)+tc.status, func(t *testing.T) {
			if got := outcomeSignalStatus(model.OutcomeSignal{Kind: tc.kind, Status: tc.status}); got != tc.want {
				t.Fatalf("got %s want %s", got, tc.want)
			}
		})
	}
}

func TestOutcomeSummaryLatestLifecycle(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []model.OutcomeSignalKind{model.OutcomeSignalIncident, model.OutcomeSignalRollback} {
		t.Run(string(kind), func(t *testing.T) {
			old := model.OutcomeSignal{ID: "a", Source: "ops", ChangeID: "change", Kind: kind, IncidentID: "entity", OperationID: "entity", OccurredAt: now, ReceivedAt: now, Status: "OPEN"}
			if kind == model.OutcomeSignalRollback {
				old.Status = "STARTED"
			}
			newer := old
			newer.ID, newer.OccurredAt = "b", now.Add(time.Minute)
			newer.Status = "RESOLVED"
			if kind == model.OutcomeSignalRollback {
				newer.Status = "SUCCEEDED"
			}
			for _, signals := range [][]model.OutcomeSignal{{old, newer}, {newer, old}} {
				before := append([]model.OutcomeSignal(nil), signals...)
				if got := summarizeOutcomeSignals(signals); got != "PASS" {
					t.Fatalf("lifecycle: %s", got)
				}
				if !reflect.DeepEqual(signals, before) {
					t.Fatal("history modified")
				}
			}
			for _, tie := range []string{"received", "id"} {
				newer.OccurredAt = old.OccurredAt
				newer.ReceivedAt = old.ReceivedAt
				if tie == "received" {
					newer.ReceivedAt = now.Add(time.Second)
					newer.ID = "0"
				} else {
					newer.ID = "b"
				}
				for _, signals := range [][]model.OutcomeSignal{{old, newer}, {newer, old}} {
					if got := summarizeOutcomeSignals(signals); got != "PASS" {
						t.Fatalf("tie %s: %s", tie, got)
					}
				}
			}
			newer.OccurredAt = now.Add(time.Minute)
			for _, field := range []string{"source", "entity", "missing", "missing_source"} {
				a, b := old, newer
				switch field {
				case "source":
					b.Source = "other"
				case "entity":
					b.IncidentID, b.OperationID = "other", "other"
				case "missing":
					a.IncidentID, a.OperationID, b.IncidentID, b.OperationID = "", "", "", ""
				case "missing_source":
					a.Source, b.Source = "", ""
				}
				if got := latestOutcomeSignals([]model.OutcomeSignal{a, b}); len(got) != 2 {
					t.Fatalf("merged %s: %+v", field, got)
				}
			}
			newer.Status = "UNKNOWN"
			if got := summarizeOutcomeSignals([]model.OutcomeSignal{old, newer}); got != "WARN" {
				t.Fatalf("unknown latest: %s", got)
			}
		})
	}
}

func TestLatestOutcomeSLIIdentity(t *testing.T) {
	now := time.Now().UTC()
	start, end := now.Add(-time.Hour), now
	base := model.OutcomeSignal{ID: "a", Source: "metrics", ChangeID: "change", Kind: model.OutcomeSignalBusinessSLI, MetricName: "latency", ObservationWindowStart: &start, ObservationWindowEnd: &end, OccurredAt: now}
	for _, tc := range []struct {
		name  string
		edit  func(*model.OutcomeSignal)
		count int
	}{
		{"same", func(s *model.OutcomeSignal) { s.MetricName = "LATENCY" }, 1},
		{"metric", func(s *model.OutcomeSignal) { s.MetricName = "errors" }, 2},
		{"source", func(s *model.OutcomeSignal) { s.Source = "other" }, 2},
		{"window", func(s *model.OutcomeSignal) { v := end.Add(time.Minute); s.ObservationWindowEnd = &v }, 2},
		{"missing_window", func(s *model.OutcomeSignal) { s.ObservationWindowStart = nil }, 2},
		{"missing_metric", func(s *model.OutcomeSignal) { s.MetricName = "" }, 2},
		{"kind", func(s *model.OutcomeSignal) { s.Kind = model.OutcomeSignalIncident; s.IncidentID = "latency" }, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newer := base
			newer.ID = "b"
			newer.OccurredAt = now.Add(time.Minute)
			tc.edit(&newer)
			for _, signals := range [][]model.OutcomeSignal{{base, newer}, {newer, base}} {
				got := latestOutcomeSignals(signals)
				if len(got) != tc.count {
					t.Fatalf("got %d want %d", len(got), tc.count)
				}
				if tc.count == 1 && got[0].ID != "b" {
					t.Fatal("did not select latest")
				}
			}
		})
	}
}
