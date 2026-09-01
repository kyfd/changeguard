package report

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func monthlyTestEvents() []model.AuditEvent {
	// 统一用 UTC 构造：8 月内 3 条、7 月 1 条、9 月 1 条
	at := func(day int, hour int) time.Time {
		return time.Date(2026, 8, day, hour, 0, 0, 0, time.UTC)
	}
	return []model.AuditEvent{
		{ID: "a1", OrganizationID: "org_1", ChangeID: "chg_1", ActorID: "usr_1", ActorName: "刘工",
			Action: "CREATE", Result: "SUCCESS", Detail: "创建研发发布变更单", CreatedAt: at(3, 2), Hash: "h1"},
		{ID: "a2", OrganizationID: "org_1", ChangeID: "chg_1", ActorID: "usr_2", ActorName: "陈静",
			Action: "APPROVE", Result: "SUCCESS", Detail: "独立审批通过", CreatedAt: at(12, 8), Hash: "h2"},
		{ID: "a3", OrganizationID: "org_1", ActorID: "system:purge", ActorName: "系统清理", ActorType: "SYSTEM",
			Action: "PASSPORTS_REVOKED", Result: "SUCCESS", Detail: "批量吊销过期通行证", CreatedAt: at(31, 15), Hash: "h3"},
		{ID: "x7", OrganizationID: "org_1", ChangeID: "chg_0", ActorID: "usr_1", ActorName: "刘工",
			Action: "CREATE", Detail: "七月的事件不应出现", CreatedAt: time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC), Hash: "hx"},
		{ID: "x9", OrganizationID: "org_1", ChangeID: "chg_9", ActorID: "usr_1", ActorName: "刘工",
			Action: "CREATE", Detail: "九月的事件不应出现", CreatedAt: time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC), Hash: "hy"},
	}
}

func monthlyTestInput(events []model.AuditEvent) MonthlyAuditInput {
	return MonthlyAuditInput{
		OrganizationName: "核心交易库",
		GeneratedBy:      "合规员",
		GeneratedRole:    "OWNER",
		GeneratedAt:      time.Date(2026, 9, 1, 6, 0, 0, 0, time.UTC),
		Month:            time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC),
		Audits:           events,
	}
}

func TestMonthlyAuditFiltersAndOrders(t *testing.T) {
	document, err := MonthlyAudit(monthlyTestInput(monthlyTestEvents()))
	if err != nil {
		t.Fatalf("MonthlyAudit: %v", err)
	}
	html := string(document)

	for _, want := range []string{"2026年08月", "核心交易库", "创建研发发布变更单", "独立审批通过", "批量吊销过期通行证"} {
		if !strings.Contains(html, want) {
			t.Fatalf("report missing %q", want)
		}
	}
	if strings.Contains(html, "七月的事件不应出现") || strings.Contains(html, "九月的事件不应出现") {
		t.Fatalf("events outside the month leaked into the report")
	}
	// 明细按时间正序：创建(01-03 UTC+8 为 08-03) 在审批之前
	createAt := strings.Index(html, "创建研发发布变更单")
	approveAt := strings.Index(html, "独立审批通过")
	if createAt == -1 || approveAt == -1 || createAt > approveAt {
		t.Fatalf("expected ascending order: create=%d approve=%d", createAt, approveAt)
	}
	// 系统操作人带标记，动作使用中文标签
	if !strings.Contains(html, "系统清理") || !strings.Contains(html, "系统") {
		t.Fatalf("system actor tag missing")
	}
	if !strings.Contains(html, "批量吊销通行证") {
		t.Fatalf("known action should use its Chinese label")
	}
	// 概览计数：3 条事件、2 张变更单、3 个操作人
	if !strings.Contains(html, "涉及变更单") || !strings.Contains(html, "2026-08-01 00:00:00") || !strings.Contains(html, "2026-08-31 23:59:59") {
		head := html
		if idx := strings.Index(html, "统计期间"); idx >= 0 {
			end := idx + 160
			if end > len(html) {
				end = len(html)
			}
			head = html[idx:end]
		}
		t.Fatalf("period header missing; meta=%q", head)
	}
	if !strings.Contains(html, "h1") || !strings.Contains(html, "h3") {
		t.Fatalf("chain hash range missing")
	}
}

func TestMonthlyAuditEscapesDetail(t *testing.T) {
	events := []model.AuditEvent{{
		ID: "a1", OrganizationID: "org_1", ActorID: "usr_1", ActorName: "<img src=x onerror=alert(1)>",
		Action: "CREATE", Detail: `<script>alert("xss")</script>`,
		CreatedAt: time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC), Hash: "h1",
	}}
	document, err := MonthlyAudit(monthlyTestInput(events))
	if err != nil {
		t.Fatalf("MonthlyAudit: %v", err)
	}
	html := string(document)
	if strings.Contains(html, "<script>alert") {
		t.Fatalf("detail was not escaped")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Fatalf("expected escaped detail text")
	}
	if regexp.MustCompile(`<img[^>]+onerror`).MatchString(html) {
		t.Fatalf("actor name was not escaped")
	}
}

func TestMonthlyAuditEmptyMonth(t *testing.T) {
	document, err := MonthlyAudit(monthlyTestInput(nil))
	if err != nil {
		t.Fatalf("MonthlyAudit: %v", err)
	}
	html := string(document)
	if !strings.Contains(html, "本统计期间内无审计事件") {
		t.Fatalf("empty-month notice missing")
	}
	if !strings.Contains(html, "合规审核人") || !strings.Contains(html, "数据库负责人") {
		t.Fatalf("signoff block must exist even for empty months")
	}
	if strings.Contains(html, "起始事件哈希") {
		t.Fatalf("empty month should not claim chain hashes")
	}
}

func TestMonthlyAuditUnknownActionKeptRaw(t *testing.T) {
	events := []model.AuditEvent{{
		ID: "a1", OrganizationID: "org_1", ActorID: "usr_1", ActorName: "刘工",
		Action: "FUTURE_THING", Detail: "未知动作不丢",
		CreatedAt: time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC), Hash: "h1",
	}}
	document, err := MonthlyAudit(monthlyTestInput(events))
	if err != nil {
		t.Fatalf("MonthlyAudit: %v", err)
	}
	if !strings.Contains(string(document), "FUTURE_THING") {
		t.Fatalf("unknown action code must be preserved verbatim")
	}
}

func TestParseMonth(t *testing.T) {
	month, err := ParseMonth(" 2026-08 ")
	if err != nil {
		t.Fatalf("ParseMonth: %v", err)
	}
	if month.Year() != 2026 || month.Month() != time.August || month.Day() != 1 {
		t.Fatalf("unexpected month start: %v", month)
	}
	if _, err := ParseMonth("2026-8"); err == nil {
		t.Fatal("single-digit month must be rejected")
	}
	if _, err := ParseMonth("not-a-month"); err == nil {
		t.Fatal("garbage must be rejected")
	}
}
