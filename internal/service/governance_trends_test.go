package service

import (
	"math"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func trendsAt(day int, hour int) time.Time {
	// 2026-08，UTC；对应 UTC+8 为同日 15:00/同日+1 凌晨，用于检验月口径
	return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC)
}

func TestGovernanceTrendsBucketsAndRates(t *testing.T) {
	changes := []model.ChangeRequest{
		// 8 月：完成 1、拒绝 1、进行中 1，其中高危 1
		{ID: "c1", CreatedAt: trendsAt(3, 2), Status: model.StatusCompleted, Risk: model.RiskLow,
			Timeline: []model.TimelineEntry{
				{Status: model.StatusWaitingApproval, CreatedAt: trendsAt(3, 4)},
				{Status: model.StatusApproved, CreatedAt: trendsAt(3, 7)},
			}},
		{ID: "c2", CreatedAt: trendsAt(10, 6), Status: model.StatusRejected, Risk: model.RiskHigh,
			Timeline: []model.TimelineEntry{
				{Status: model.StatusWaitingApproval, CreatedAt: trendsAt(10, 6)},
				{Status: model.StatusRejected, CreatedAt: trendsAt(11, 6)},
			}},
		{ID: "c3", CreatedAt: trendsAt(20, 18), Status: model.StatusWaitingApproval, Risk: model.RiskMedium},
		// 7 月：仅有一条已完成（窗口外，不应计入 8 月）
		{ID: "c0", CreatedAt: time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC), Status: model.StatusCompleted, Risk: model.RiskLow},
	}

	now := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC) // UTC+8 已是 9 月 1 日
	trends := governanceTrends(changes, 2, now)

	if len(trends) != 2 {
		t.Fatalf("expected 2 buckets, got %d: %+v", len(trends), trends)
	}
	august := trends[0]
	if august.Month != "2026-08" {
		t.Fatalf("first bucket should be 2026-08, got %s", august.Month)
	}
	if august.Submitted != 3 || august.Completed != 1 || august.Rejected != 1 || august.InFlight != 1 {
		t.Fatalf("august counts wrong: %+v", august)
	}
	if august.HighRisk != 1 {
		t.Fatalf("august high risk wrong: %+v", august)
	}
	if math.Abs(august.RejectionRate-0.5) > 1e-9 {
		t.Fatalf("august rejection rate should be 0.5, got %v", august.RejectionRate)
	}
	if math.Abs(august.HighRiskRate-1.0/3.0) > 1e-9 {
		t.Fatalf("august high risk rate should be 1/3, got %v", august.HighRiskRate)
	}
	// 审批时长：c1 = 3h，c2 = 24h → 均值 13.5h
	if math.Abs(august.ApprovalHours-13.5) > 1e-9 {
		t.Fatalf("august approval hours should be 13.5, got %v", august.ApprovalHours)
	}

	september := trends[1]
	if september.Month != "2026-09" {
		t.Fatalf("second bucket should be 2026-09, got %s", september.Month)
	}
	if september.Submitted != 0 || september.RejectionRate != -1 || september.HighRiskRate != -1 || september.ApprovalHours != -1 {
		t.Fatalf("empty month should be zero with -1 sentinels: %+v", september)
	}
}

func TestGovernanceTrendsMonthBoundaryUTCPlus8(t *testing.T) {
	// UTC 8/31 20:00 = UTC+8 9/1 04:00 → 必须落在 9 月桶
	changes := []model.ChangeRequest{
		{ID: "c1", CreatedAt: time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC), Status: model.StatusCompleted, Risk: model.RiskLow},
	}
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	trends := governanceTrends(changes, 2, now)
	if trends[0].Month != "2026-08" || trends[0].Submitted != 0 {
		t.Fatalf("august bucket should be empty: %+v", trends[0])
	}
	if trends[1].Month != "2026-09" || trends[1].Submitted != 1 || trends[1].Completed != 1 {
		t.Fatalf("september bucket wrong: %+v", trends[1])
	}
}

func TestGovernanceTrendsLatencyFallbacks(t *testing.T) {
	changes := []model.ChangeRequest{
		// 无 WAITING_APPROVAL 前导：回退用 CreatedAt 起算
		{ID: "c1", CreatedAt: trendsAt(2, 0), Status: model.StatusApproved, Risk: model.RiskLow,
			Timeline: []model.TimelineEntry{{Status: model.StatusApproved, CreatedAt: trendsAt(2, 5)}}},
		// 只有定论前的时间倒挂：不计入
		{ID: "c2", CreatedAt: trendsAt(3, 0), Status: model.StatusRejected, Risk: model.RiskLow,
			Timeline: []model.TimelineEntry{
				{Status: model.StatusWaitingApproval, CreatedAt: trendsAt(4, 0)},
				{Status: model.StatusRejected, CreatedAt: trendsAt(3, 12)},
			}},
		// 进行中：无定论，不计入
		{ID: "c3", CreatedAt: trendsAt(5, 0), Status: model.StatusWaitingApproval, Risk: model.RiskLow,
			Timeline: []model.TimelineEntry{{Status: model.StatusWaitingApproval, CreatedAt: trendsAt(5, 2)}}},
	}
	trends := governanceTrends(changes, 1, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	august := trends[0]
	if august.ApprovalHours != 5 {
		t.Fatalf("approval hours should average only the valid sample (5h), got %v", august.ApprovalHours)
	}
	// c1 已批准未执行、c3 待审批 → 都算推进中；c2 虽然时间倒挂但状态已定局
	if august.InFlight != 2 {
		t.Fatalf("approved-but-not-run and waiting changes should be in flight: %+v", august)
	}
}

func TestGovernanceTrendsDefaultsToSixMonths(t *testing.T) {
	now := time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	trends := governanceTrends(nil, 0, now)
	if len(trends) != 6 {
		t.Fatalf("months<=0 should default to 6, got %d", len(trends))
	}
	if trends[5].Month != "2026-08" || trends[0].Month != "2026-03" {
		t.Fatalf("unexpected window: %+v", trends)
	}
}
