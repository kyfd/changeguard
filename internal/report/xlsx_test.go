package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

func TestXLSXContainsEnterpriseEvidenceSheets(t *testing.T) {
	now := time.Now()
	change := model.ChangeRequest{
		ID: "chg_test", Title: "订单表索引优化", ApplicationName: "订单中心",
		Environment: "生产环境", ChangeType: "DDL", Status: model.StatusWaitingApproval,
		Risk: model.RiskMedium, SubmitterName: "刘丰熙", CreatedAt: now, UpdatedAt: now,
		PlannedAt: now.Add(time.Hour), SQL: "CREATE INDEX idx_orders_status ON orders(status);",
		RollbackSQL: "DROP INDEX idx_orders_status;",
		Findings:    []model.Finding{{ID: "ev_1", Code: "INDEX_NOT_CONCURRENT", Severity: model.RiskMedium, Title: "索引未并发创建", Status: model.FindingVerified, Resolution: "已调整为并发创建", VerifiedByName: "数据库审核人", UpdatedAt: now}},
		Timeline:    []model.TimelineEntry{{ID: "tl_1", Status: model.StatusDraft, Title: "创建变更单", Actor: "刘丰熙", CreatedAt: now}},
	}
	content, err := XLSX(change, []model.AuditEvent{{ID: "audit_1", ChangeID: change.ID, Action: "CREATE", ActorName: "刘丰熙", CreatedAt: now}})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatalf("invalid xlsx zip: %v", err)
	}
	if len(reader.File) < 10 {
		t.Fatalf("xlsx package is incomplete: %d files", len(reader.File))
	}
	var workbook string
	for _, file := range reader.File {
		if file.Name != "xl/workbook.xml" {
			continue
		}
		handle, openErr := file.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		data, readErr := io.ReadAll(handle)
		_ = handle.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		workbook = string(data)
	}
	for _, sheetName := range []string{"变更概览", "变更制品", "规则证据", "预发布验证", "Agent分析", "审批时间线", "审计日志"} {
		if !strings.Contains(workbook, sheetName) {
			t.Fatalf("workbook missing sheet %s", sheetName)
		}
	}
}

func TestXLSXRemovesInvalidXMLControlCharacters(t *testing.T) {
	change := model.ChangeRequest{
		ID: "chg_control", Title: "订单\x00变更", Description: "包含\x01控制字符",
		CreatedAt: time.Now(), UpdatedAt: time.Now(), PlannedAt: time.Now().Add(time.Hour),
		SQL: "SELECT 1;", RollbackSQL: "SELECT 1;",
	}
	content, err := XLSX(change, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range reader.File {
		if !strings.HasSuffix(file.Name, ".xml") {
			continue
		}
		handle, openErr := file.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		data, readErr := io.ReadAll(handle)
		_ = handle.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		var target struct{ XMLName xml.Name }
		if parseErr := xml.Unmarshal(data, &target); parseErr != nil {
			t.Fatalf("invalid XML in %s: %v", file.Name, parseErr)
		}
	}
}
