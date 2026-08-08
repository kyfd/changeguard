package report

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/liufengxi/dbguard/internal/model"
)

type cell struct {
	Value  string
	Style  int
	Number bool
}

type sheet struct {
	Name   string
	Rows   [][]cell
	Widths []float64
}

func XLSX(change model.ChangeRequest, audits []model.AuditEvent) ([]byte, error) {
	sheets := []sheet{
		overviewSheet(change),
		artifactsSheet(change),
		findingsSheet(change),
		experimentSheet(change),
		analysisSheet(change),
		timelineSheet(change),
		auditSheet(change, audits),
	}
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	files := map[string]string{
		"[Content_Types].xml":        contentTypes(len(sheets)),
		"_rels/.rels":                rootRelationships,
		"docProps/app.xml":           appProperties(sheets),
		"docProps/core.xml":          coreProperties(change),
		"xl/workbook.xml":            workbookXML(sheets),
		"xl/_rels/workbook.xml.rels": workbookRelationships(len(sheets)),
		"xl/styles.xml":              stylesXML,
	}
	for index, item := range sheets {
		files[fmt.Sprintf("xl/worksheets/sheet%d.xml", index+1)] = sheetXML(item)
	}
	for name, content := range files {
		writer, err := archive.Create(name)
		if err != nil {
			return nil, err
		}
		if _, err := writer.Write([]byte(content)); err != nil {
			return nil, err
		}
	}
	if err := archive.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func overviewSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("ChangeGuard 企业研发变更证据报告", change)
	rows = append(rows,
		row("字段", "内容"),
		row("变更编号", change.ID),
		row("变更标题", change.Title),
		row("所属服务", change.ApplicationName),
		row("目标环境", change.Environment),
		row("变更类型", change.ChangeType),
		row("代码仓库", empty(change.RepositoryURL)),
		row("分支", empty(change.Branch)),
		row("Commit SHA", empty(change.CommitSHA)),
		row("当前状态", string(change.Status)),
		row("综合风险", string(change.Risk)),
		row("提交人", change.SubmitterName),
		row("审批人", empty(change.ReviewerName)),
		row("计划窗口", formatTime(change.PlannedAt)),
		row("创建时间", formatTime(change.CreatedAt)),
		row("更新时间", formatTime(change.UpdatedAt)),
		row("业务说明", empty(change.Description)),
		row("整体回滚方案", empty(change.RollbackPlan)),
		row("发布策略", empty(change.ReleasePlan.Strategy)),
		row("首批流量比例", fmt.Sprintf("%d%%", change.ReleasePlan.CanaryPercent)),
		row("观察窗口", fmt.Sprintf("%d 分钟", change.ReleasePlan.ObservationMinutes)),
		row("异常处理", map[bool]string{true: "允许自动中止", false: "人工决策"}[change.ReleasePlan.AutoRollback]),
		row("成功判定指标", empty(strings.Join(change.ReleasePlan.SuccessMetrics, "、"))),
		row("执行 SQL", change.SQL),
		row("回滚 SQL", change.RollbackSQL),
		row("审批意见", empty(change.ReviewComment)),
	)
	for i := range rows {
		if i >= 3 {
			rows[i][0].Style = 2
			rows[i][1].Style = 3
		}
	}
	return sheet{Name: "变更概览", Rows: rows, Widths: []float64{22, 92}}
}

func artifactsSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("变更制品证据", change)
	rows = append(rows, cells("制品编号", "制品类型", "制品名称", "来源", "语言 / 格式", "内容证据"))
	for i := range rows[3] {
		rows[3][i].Style = 2
	}
	artifacts := append([]model.ChangeArtifact(nil), change.Artifacts...)
	if change.SQL != "" {
		hasDatabase := false
		for _, artifact := range artifacts {
			if artifact.Kind == model.ArtifactDatabase {
				hasDatabase = true
				break
			}
		}
		if !hasDatabase {
			artifacts = append(artifacts, model.ChangeArtifact{Kind: model.ArtifactDatabase, Name: "数据库 SQL", Language: "SQL", Content: change.SQL})
		}
	}
	if len(artifacts) == 0 {
		rows = append(rows, cells("—", "—", "未登记制品", "—", "—", "—"))
	}
	for _, artifact := range artifacts {
		rowCells := cells(empty(artifact.ID), string(artifact.Kind), empty(artifact.Name), empty(artifact.Source), empty(artifact.Language), empty(artifact.Content))
		for i := range rowCells {
			rowCells[i].Style = 3
		}
		rows = append(rows, rowCells)
	}
	return sheet{Name: "变更制品", Rows: rows, Widths: []float64{22, 18, 28, 24, 18, 100}}
}

func findingsSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("规则证据与整改闭环", change)
	rows = append(rows, cells("证据编号", "规则编码", "风险级别", "风险标题", "证据内容", "整改建议", "闭环状态", "负责人", "整改期限", "整改说明", "复核人", "复核意见", "复核时间"))
	for i := range rows[3] {
		rows[3][i].Style = 2
	}
	for _, finding := range change.Findings {
		style := riskStyle(finding.Severity)
		status := string(finding.Status)
		if status == "" {
			status = string(model.FindingOpen)
		}
		rowCells := cells(finding.ID, finding.Code, string(finding.Severity), finding.Title, finding.Evidence, finding.Suggestion, status, empty(finding.OwnerName), formatTimePtr(finding.DueAt), empty(finding.Resolution), empty(finding.VerifiedByName), empty(finding.VerificationComment), formatTimePtr(finding.VerifiedAt))
		for i := range rowCells {
			rowCells[i].Style = 3
		}
		rowCells[2].Style = style
		rows = append(rows, rowCells)
	}
	return sheet{Name: "规则证据", Rows: rows, Widths: []float64{24, 25, 12, 32, 55, 55, 16, 16, 20, 55, 16, 45, 20}}
}

func experimentSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("预发布验证证据", change)
	rows = append(rows, row("指标", "结果"))
	rows[3][0].Style, rows[3][1].Style = 2, 2
	if change.Experiment == nil {
		rows = append(rows, row("验证状态", "尚未执行"))
	} else {
		exp := change.Experiment
		rows = append(rows,
			row("验证编号", exp.ID),
			row("执行模式", exp.Mode),
			row("验证结果", exp.Status),
			row("验证类型", exp.Kind),
			numberRow("验证项总数", int64(exp.ChecksTotal)),
			numberRow("通过项数", int64(exp.ChecksPassed)),
			row("发布策略", exp.Strategy),
			numberRow("首批流量比例(%)", int64(exp.CanaryPercent)),
			numberRow("观察窗口(分钟)", int64(exp.ObservationMinutes)),
			row("开始时间", formatTime(exp.StartedAt)),
			row("完成时间", formatTime(exp.FinishedAt)),
			numberRow("执行耗时(ms)", exp.DurationMS),
			numberRow("样本数据量", exp.DatasetRows),
			numberRow("最大锁等待(ms)", exp.LockWaitMS),
			floatRow("变更前 P99(ms)", exp.P99BeforeMS),
			floatRow("变更后 P99(ms)", exp.P99AfterMS),
			numberRow("失败事务数", int64(exp.FailedTransactions)),
			row("回滚验证", boolText(exp.RollbackVerified)),
			row("执行错误", empty(exp.ExecutionError)),
		)
		for _, evidence := range exp.Evidence {
			rows = append(rows, row("验证证据 · "+evidence.Title, evidence.Value+" ｜ 来源："+evidence.Source+" ｜ "+formatTime(evidence.ObservedAt)))
		}
	}
	for i := 4; i < len(rows); i++ {
		rows[i][0].Style = 2
		rows[i][1].Style = 3
	}
	return sheet{Name: "预发布验证", Rows: rows, Widths: []float64{28, 92}}
}

func analysisSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("Agent 只读证据分析", change)
	rows = append(rows, row("字段", "内容"))
	rows[3][0].Style, rows[3][1].Style = 2, 2
	if change.Analysis == nil {
		rows = append(rows, row("分析状态", "等待预发布验证证据"))
	} else {
		analysis := change.Analysis
		rows = append(rows,
			row("服务商", analysis.Provider),
			row("模型", analysis.Model),
			row("风险判断", string(analysis.Risk)),
			row("分析结论", analysis.Summary),
			row("主要依据", strings.Join(analysis.Reasons, "\n")),
			row("上线建议", strings.Join(analysis.Suggestions, "\n")),
			row("引用证据", strings.Join(analysis.EvidenceIDs, "\n")),
			numberRow("推理步数", int64(analysis.Steps)),
			numberRow("工具调用", int64(analysis.ToolCalls)),
			numberRow("输出 Token", int64(analysis.Tokens)),
			row("生成时间", formatTime(analysis.GeneratedAt)),
		)
	}
	for i := 4; i < len(rows); i++ {
		rows[i][0].Style = 2
		rows[i][1].Style = 3
	}
	return sheet{Name: "Agent分析", Rows: rows, Widths: []float64{24, 92}}
}

func timelineSheet(change model.ChangeRequest) sheet {
	rows := reportHeader("审批与处理时间线", change)
	rows = append(rows, cells("记录编号", "状态", "事件", "详细说明", "操作人", "发生时间"))
	for i := range rows[3] {
		rows[3][i].Style = 2
	}
	for _, item := range change.Timeline {
		line := cells(item.ID, string(item.Status), item.Title, item.Detail, item.Actor, formatTime(item.CreatedAt))
		for i := range line {
			line[i].Style = 3
		}
		rows = append(rows, line)
	}
	return sheet{Name: "审批时间线", Rows: rows, Widths: []float64{24, 22, 28, 70, 18, 22}}
}

func auditSheet(change model.ChangeRequest, audits []model.AuditEvent) sheet {
	rows := reportHeader("审计日志", change)
	rows = append(rows, cells("审计编号", "变更编号", "操作编码", "操作详情", "操作人", "操作人编号", "发生时间"))
	for i := range rows[3] {
		rows[3][i].Style = 2
	}
	for _, item := range audits {
		if item.ChangeID != change.ID {
			continue
		}
		line := cells(item.ID, item.ChangeID, item.Action, item.Detail, item.ActorName, item.ActorID, formatTime(item.CreatedAt))
		for i := range line {
			line[i].Style = 3
		}
		rows = append(rows, line)
	}
	return sheet{Name: "审计日志", Rows: rows, Widths: []float64{24, 24, 24, 70, 18, 22, 22}}
}

func reportHeader(title string, change model.ChangeRequest) [][]cell {
	return [][]cell{
		{{Value: title, Style: 1}},
		{{Value: "报告生成时间", Style: 2}, {Value: formatTime(time.Now()), Style: 3}},
		{{Value: "变更编号", Style: 2}, {Value: change.ID, Style: 3}},
	}
}

func row(label, value string) []cell {
	return []cell{{Value: label}, {Value: value}}
}

func cells(values ...string) []cell {
	result := make([]cell, len(values))
	for i, value := range values {
		result[i] = cell{Value: value}
	}
	return result
}

func numberRow(label string, value int64) []cell {
	return []cell{{Value: label}, {Value: strconv.FormatInt(value, 10), Number: true}}
}

func floatRow(label string, value float64) []cell {
	return []cell{{Value: label}, {Value: strconv.FormatFloat(value, 'f', 2, 64), Number: true}}
}

func boolText(value bool) string {
	if value {
		return "通过"
	}
	return "未通过"
}

func empty(value string) string {
	if strings.TrimSpace(value) == "" {
		return "—"
	}
	return value
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Local().Format("2006-01-02 15:04:05")
}

func formatTimePtr(value *time.Time) string {
	if value == nil {
		return "—"
	}
	return formatTime(*value)
}

func riskStyle(level model.RiskLevel) int {
	if level == model.RiskHigh {
		return 4
	}
	if level == model.RiskMedium {
		return 5
	}
	return 6
}

func sheetXML(item sheet) string {
	var body strings.Builder
	body.WriteString(xmlHeader)
	body.WriteString(`<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetViews><sheetView workbookViewId="0"><pane ySplit="4" topLeftCell="A5" activePane="bottomLeft" state="frozen"/></sheetView></sheetViews><cols>`)
	for index, width := range item.Widths {
		fmt.Fprintf(&body, `<col min="%d" max="%d" width="%.1f" customWidth="1"/>`, index+1, index+1, width)
	}
	body.WriteString("</cols><sheetData>")
	for rowIndex, rowCells := range item.Rows {
		fmt.Fprintf(&body, `<row r="%d">`, rowIndex+1)
		for columnIndex, itemCell := range rowCells {
			ref := columnName(columnIndex+1) + strconv.Itoa(rowIndex+1)
			if itemCell.Number {
				fmt.Fprintf(&body, `<c r="%s" s="%d"><v>%s</v></c>`, ref, itemCell.Style, escape(itemCell.Value))
			} else {
				fmt.Fprintf(&body, `<c r="%s" s="%d" t="inlineStr"><is><t xml:space="preserve">%s</t></is></c>`, ref, itemCell.Style, escape(itemCell.Value))
			}
		}
		body.WriteString("</row>")
	}
	body.WriteString(`</sheetData><autoFilter ref="A4:` + columnName(maxColumns(item.Rows)) + `4"/></worksheet>`)
	return body.String()
}

func maxColumns(rows [][]cell) int {
	result := 1
	for _, row := range rows {
		if len(row) > result {
			result = len(row)
		}
	}
	return result
}

func columnName(value int) string {
	var result string
	for value > 0 {
		value--
		result = string(rune('A'+value%26)) + result
		value /= 26
	}
	return result
}

func escape(value string) string {
	value = strings.Map(func(item rune) rune {
		if item == 0x9 || item == 0xA || item == 0xD ||
			(item >= 0x20 && item <= 0xD7FF) ||
			(item >= 0xE000 && item <= 0xFFFD) ||
			(item >= 0x10000 && item <= 0x10FFFF) {
			return item
		}
		return -1
	}, value)
	var body bytes.Buffer
	_ = xml.EscapeText(&body, []byte(value))
	return body.String()
}

func contentTypes(count int) string {
	var body strings.Builder
	body.WriteString(xmlHeader + `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/><Override PartName="/docProps/core.xml" ContentType="application/vnd.openxmlformats-package.core-properties+xml"/><Override PartName="/docProps/app.xml" ContentType="application/vnd.openxmlformats-officedocument.extended-properties+xml"/>`)
	for index := 1; index <= count; index++ {
		fmt.Fprintf(&body, `<Override PartName="/xl/worksheets/sheet%d.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>`, index)
	}
	body.WriteString("</Types>")
	return body.String()
}

func workbookXML(sheets []sheet) string {
	var body strings.Builder
	body.WriteString(xmlHeader + `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets>`)
	for index, item := range sheets {
		fmt.Fprintf(&body, `<sheet name="%s" sheetId="%d" r:id="rId%d"/>`, escape(item.Name), index+1, index+1)
	}
	body.WriteString("</sheets></workbook>")
	return body.String()
}

func workbookRelationships(count int) string {
	var body strings.Builder
	body.WriteString(xmlHeader + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	for index := 1; index <= count; index++ {
		fmt.Fprintf(&body, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet%d.xml"/>`, index, index)
	}
	fmt.Fprintf(&body, `<Relationship Id="rId%d" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>`, count+1)
	body.WriteString("</Relationships>")
	return body.String()
}

func appProperties(sheets []sheet) string {
	var titles strings.Builder
	for _, item := range sheets {
		titles.WriteString("<vt:lpstr>" + escape(item.Name) + "</vt:lpstr>")
	}
	return xmlHeader + `<Properties xmlns="http://schemas.openxmlformats.org/officeDocument/2006/extended-properties" xmlns:vt="http://schemas.openxmlformats.org/officeDocument/2006/docPropsVTypes"><Application>DBGuard</Application><TitlesOfParts><vt:vector size="` + strconv.Itoa(len(sheets)) + `" baseType="lpstr">` + titles.String() + `</vt:vector></TitlesOfParts></Properties>`
}

func coreProperties(change model.ChangeRequest) string {
	return xmlHeader + `<cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties" xmlns:dc="http://purl.org/dc/elements/1.1/" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><dc:title>` + escape(change.Title) + `</dc:title><dc:creator>DBGuard</dc:creator><dcterms:created xsi:type="dcterms:W3CDTF">` + time.Now().UTC().Format(time.RFC3339) + `</dcterms:created></cp:coreProperties>`
}

const xmlHeader = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>`

const rootRelationships = xmlHeader + `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/><Relationship Id="rId2" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties" Target="docProps/core.xml"/><Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties" Target="docProps/app.xml"/></Relationships>`

const stylesXML = xmlHeader + `<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><fonts count="3"><font><sz val="10"/><name val="Microsoft YaHei"/></font><font><b/><sz val="16"/><color rgb="FF1F2937"/><name val="Microsoft YaHei"/></font><font><b/><sz val="10"/><color rgb="FFFFFFFF"/><name val="Microsoft YaHei"/></font></fonts><fills count="6"><fill><patternFill patternType="none"/></fill><fill><patternFill patternType="gray125"/></fill><fill><patternFill patternType="solid"><fgColor rgb="FF2458E6"/><bgColor indexed="64"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFFFE4E6"/><bgColor indexed="64"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFFFF2D6"/><bgColor indexed="64"/></patternFill></fill><fill><patternFill patternType="solid"><fgColor rgb="FFE4F7ED"/><bgColor indexed="64"/></patternFill></fill></fills><borders count="2"><border/><border><left style="thin"><color rgb="FFE5E7EB"/></left><right style="thin"><color rgb="FFE5E7EB"/></right><top style="thin"><color rgb="FFE5E7EB"/></top><bottom style="thin"><color rgb="FFE5E7EB"/></bottom></border></borders><cellStyleXfs count="1"><xf numFmtId="0" fontId="0" fillId="0" borderId="0"/></cellStyleXfs><cellXfs count="7"><xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0" applyAlignment="1"><alignment vertical="top"/></xf><xf numFmtId="0" fontId="1" fillId="0" borderId="0" xfId="0"/><xf numFmtId="0" fontId="2" fillId="2" borderId="1" xfId="0" applyAlignment="1"><alignment vertical="center"/></xf><xf numFmtId="0" fontId="0" fillId="0" borderId="1" xfId="0" applyAlignment="1"><alignment wrapText="1" vertical="top"/></xf><xf numFmtId="0" fontId="0" fillId="3" borderId="1" xfId="0" applyAlignment="1"><alignment horizontal="center"/></xf><xf numFmtId="0" fontId="0" fillId="4" borderId="1" xfId="0" applyAlignment="1"><alignment horizontal="center"/></xf><xf numFmtId="0" fontId="0" fillId="5" borderId="1" xfId="0" applyAlignment="1"><alignment horizontal="center"/></xf></cellXfs><cellStyles count="1"><cellStyle name="Normal" xfId="0" builtinId="0"/></cellStyles></styleSheet>`
