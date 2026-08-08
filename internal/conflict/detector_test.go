package conflict

import (
	"testing"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

func TestDetectFindsSameApplicationAndSharedTable(t *testing.T) {
	now := time.Now().Add(time.Hour)
	changes := []model.ChangeRequest{
		{
			ID: "chg_a", Title: "expand orders", ApplicationID: "app_orders",
			ApplicationName: "orders", Environment: "production",
			Status: model.StatusWaitingApproval, Risk: model.RiskHigh,
			PlannedAt: now, SQL: "ALTER TABLE orders ADD COLUMN channel text",
			ReleasePlan: model.ReleasePlan{ObservationMinutes: 45},
		},
		{
			ID: "chg_b", Title: "backfill orders", ApplicationID: "app_orders",
			ApplicationName: "orders", Environment: "production",
			Status: model.StatusApproved, Risk: model.RiskMedium,
			PlannedAt: now.Add(15 * time.Minute), SQL: "UPDATE orders SET channel = 'web'",
			ReleasePlan: model.ReleasePlan{ObservationMinutes: 30},
		},
	}
	radar := Detect(changes, nil, now.Add(-time.Hour), now.Add(24*time.Hour))
	if radar.ConflictCount != 1 || radar.HighRiskCount != 1 {
		t.Fatalf("unexpected radar summary: %+v", radar)
	}
	conflict := radar.Conflicts[0]
	if conflict.Severity != model.RiskHigh || len(conflict.Reasons) < 2 {
		t.Fatalf("expected high conflict with multiple reasons: %+v", conflict)
	}
}

func TestDetectFindsApplicationDependency(t *testing.T) {
	now := time.Now().Add(time.Hour)
	apps := []model.Application{
		{ID: "app_api", Name: "API gateway"},
		{ID: "app_orders", Name: "orders", Dependencies: []string{"app_api"}},
	}
	changes := []model.ChangeRequest{
		{ID: "chg_api", ApplicationID: "app_api", ApplicationName: "API gateway", Environment: "production", Status: model.StatusApproved, PlannedAt: now},
		{ID: "chg_orders", ApplicationID: "app_orders", ApplicationName: "orders", Environment: "production", Status: model.StatusWaitingApproval, PlannedAt: now.Add(10 * time.Minute)},
	}
	radar := Detect(changes, apps, now.Add(-time.Hour), now.Add(24*time.Hour))
	if radar.ConflictCount != 1 || radar.Conflicts[0].Severity != model.RiskMedium {
		t.Fatalf("expected one medium dependency conflict: %+v", radar)
	}
}

func TestDetectIgnoresDifferentEnvironmentAndNonOverlappingWindows(t *testing.T) {
	now := time.Now().Add(time.Hour)
	changes := []model.ChangeRequest{
		{ID: "chg_a", ApplicationID: "app", Environment: "production", Status: model.StatusApproved, PlannedAt: now},
		{ID: "chg_b", ApplicationID: "app", Environment: "staging", Status: model.StatusApproved, PlannedAt: now.Add(5 * time.Minute)},
		{ID: "chg_c", ApplicationID: "app", Environment: "production", Status: model.StatusApproved, PlannedAt: now.Add(2 * time.Hour)},
	}
	radar := Detect(changes, nil, now.Add(-time.Hour), now.Add(24*time.Hour))
	if radar.ConflictCount != 0 {
		t.Fatalf("expected no conflicts: %+v", radar.Conflicts)
	}
}
