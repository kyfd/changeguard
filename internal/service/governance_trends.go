package service

import (
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

// trendZone 趋势月口径与审计月报一致：UTC+8 自然月。
var trendZone = time.FixedZone("UTC+8", 8*3600)

// governanceTrends 聚合最近 months 个自然月（含当前月，按 UTC+8）的治理趋势。
// 比率在无样本时返回 -1，由前端显示为"—"；审批时长取"提交 → 首次审批定论
// （通过或拒绝）"的 Timeline 区间均值。
func governanceTrends(changes []model.ChangeRequest, months int, now time.Time) []model.GovernanceTrendMonth {
	if months <= 0 {
		months = 6
	}
	local := now.In(trendZone)
	cursor := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, trendZone)

	out := make([]model.GovernanceTrendMonth, 0, months)
	index := make(map[string]*model.GovernanceTrendMonth, months)
	for i := months - 1; i >= 0; i-- {
		start := cursor.AddDate(0, -i, 0)
		out = append(out, model.GovernanceTrendMonth{
			Month:         start.Format("2006-01"),
			RejectionRate: -1,
			HighRiskRate:  -1,
			ApprovalHours: -1,
		})
		index[start.Format("2006-01")] = &out[len(out)-1]
	}
	latencies := make(map[string][]float64, months)

	for _, change := range changes {
		bucket, ok := index[change.CreatedAt.In(trendZone).Format("2006-01")]
		if !ok {
			continue
		}
		bucket.Submitted++
		switch change.Status {
		case model.StatusCompleted:
			bucket.Completed++
		case model.StatusRejected:
			bucket.Rejected++
		default:
			bucket.InFlight++
		}
		if change.Risk == model.RiskHigh {
			bucket.HighRisk++
		}
		if hours, ok := approvalLatencyHours(change); ok {
			latencies[bucket.Month] = append(latencies[bucket.Month], hours)
		}
	}

	for i := range out {
		bucket := &out[i]
		if decided := bucket.Completed + bucket.Rejected; decided > 0 {
			bucket.RejectionRate = float64(bucket.Rejected) / float64(decided)
		}
		if bucket.Submitted > 0 {
			bucket.HighRiskRate = float64(bucket.HighRisk) / float64(bucket.Submitted)
		}
		if list := latencies[bucket.Month]; len(list) > 0 {
			var total float64
			for _, hours := range list {
				total += hours
			}
			bucket.ApprovalHours = total / float64(len(list))
		}
	}
	return out
}

// approvalLatencyHours 提交（或首次进入待审批）到首次审批定论的小时数。
// 没有定论记录的变更不参与统计。
func approvalLatencyHours(change model.ChangeRequest) (float64, bool) {
	var waiting, decided time.Time
	for _, entry := range change.Timeline {
		switch entry.Status {
		case model.StatusWaitingApproval:
			if waiting.IsZero() || entry.CreatedAt.Before(waiting) {
				waiting = entry.CreatedAt
			}
		case model.StatusApproved, model.StatusRejected:
			if decided.IsZero() || entry.CreatedAt.Before(decided) {
				decided = entry.CreatedAt
			}
		}
	}
	if decided.IsZero() {
		return 0, false
	}
	if waiting.IsZero() {
		waiting = change.CreatedAt
	}
	hours := decided.Sub(waiting).Hours()
	if hours < 0 {
		return 0, false
	}
	return hours, true
}
