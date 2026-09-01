package report

import (
	"bytes"
	"html/template"
	"sort"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/model"
)

// MonthlyAudit 渲染指定自然月的审计月报 HTML。合规人员在浏览器中
// "打印 → 另存为 PDF" 即得归档文件；中文由浏览器字体渲染，服务端保持
// 与 XLSX 相同的零第三方依赖原则。页面为 A4 打印版式，明细表跨页自动重复表头。

// cnZone 审计事件以 UTC 落库，报告面向国内合规场景统一显示为 UTC+8。
var cnZone = time.FixedZone("UTC+8", 8*3600)

// actionLabels 覆盖治理主链路上的动作；未收录的动作原样输出代码，
// 保证未知动作不丢、不猜。
var actionLabels = map[string]string{
	"CREATE":                      "创建变更单",
	"UPDATE":                      "更新变更单",
	"SUBMIT":                      "提交检查",
	"APPROVE":                     "审批通过",
	"REJECT":                      "审批拒绝",
	"QUEUE_EXPERIMENT":            "发起预发验证",
	"ROLLBACK_EXECUTION_RECORDED": "记录回滚执行",
	"INCIDENT_LINKED":             "关联事故",
	"OUTCOME_SIGNAL_RECORDED":     "记录结果信号",
	"PASSPORT_REVOKED":            "吊销通行证",
	"PASSPORTS_REVOKED":           "批量吊销通行证",
	"CREATE_APPLICATION":          "纳管应用",
	"UPDATE_APPLICATION":          "更新纳管应用",
	"RETRY_OUTBOX":                "重发事件",
}

type monthlyRow struct {
	Index    int
	Time     string
	Actor    string
	ActorTag string
	Action   string
	ActionMu bool
	ChangeID string
	Detail   string
	Result   string
}

type countRow struct {
	Name  string
	Count int
}

type dayBar struct {
	Day    int
	Count  int
	Height int
}

type monthlyData struct {
	Organization  string
	GeneratedBy   string
	GeneratedRole string
	GeneratedAt   string
	PeriodStart   string
	PeriodEnd     string
	MonthLabel    string
	Total         int
	ChangeCount   int
	ActorCount    int
	ActiveDays    int
	Rows          []monthlyRow
	Actions       []countRow
	Days          []dayBar
	FirstHash     string
	LastHash      string
	Empty         bool
}

// ParseMonth 解析 "YYYY-MM"，返回该月 UTC 零点。
func ParseMonth(s string) (time.Time, error) {
	value, err := time.Parse("2006-01", strings.TrimSpace(s))
	if err != nil {
		return time.Time{}, err
	}
	return value, nil
}

type MonthlyAuditInput struct {
	OrganizationName string
	GeneratedBy      string
	GeneratedRole    string
	GeneratedAt      time.Time
	Month            time.Time // 月内任意时刻；只保留 [当月1日, 次月1日) 的事件
	Audits           []model.AuditEvent
}

func MonthlyAudit(input MonthlyAuditInput) ([]byte, error) {
	// 统计期按 UTC+8 自然月界定，与报告展示口径一致
	month := input.Month.In(cnZone)
	start := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, cnZone)
	end := start.AddDate(0, 1, 0)

	within := make([]model.AuditEvent, 0, len(input.Audits))
	for _, event := range input.Audits {
		if !event.CreatedAt.Before(start) && event.CreatedAt.Before(end) {
			within = append(within, event)
		}
	}
	// 审计链按追加序存证，报告按时间正序呈现
	sort.Slice(within, func(i, j int) bool { return within[i].CreatedAt.Before(within[j].CreatedAt) })

	data := buildMonthlyData(input, start, end, within)

	var output bytes.Buffer
	if err := monthlyTemplate.Execute(&output, data); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func buildMonthlyData(input MonthlyAuditInput, start, end time.Time, within []model.AuditEvent) monthlyData {
	data := monthlyData{
		Organization:  input.OrganizationName,
		GeneratedBy:   input.GeneratedBy,
		GeneratedRole: roleLabel(input.GeneratedRole),
		GeneratedAt:   input.GeneratedAt.In(cnZone).Format("2006-01-02 15:04:05"),
		PeriodStart:   start.Format("2006-01-02 15:04:05"),
		PeriodEnd:     end.Add(-time.Second).Format("2006-01-02 15:04:05"),
		MonthLabel:    start.Format("2006年01月"),
		Total:         len(within),
		Empty:         len(within) == 0,
	}

	changes := map[string]bool{}
	actors := map[string]bool{}
	actionCounts := map[string]int{}
	dayCounts := map[string]int{}

	for i, event := range within {
		row := monthlyRow{
			Index:    i + 1,
			Time:     event.CreatedAt.In(cnZone).Format("01-02 15:04:05"),
			Actor:    nonEmpty(event.ActorName, event.ActorID),
			ChangeID: nonEmpty(event.ChangeID, "—"),
			Detail:   event.Detail,
			Result:   resultLabel(event.Result),
		}
		if tag := actorTag(event.ActorType); tag != "" {
			row.ActorTag = tag
		}
		if label, ok := actionLabels[event.Action]; ok {
			row.Action = label
			row.ActionMu = true
		} else {
			row.Action = nonEmpty(event.Action, "—")
		}
		data.Rows = append(data.Rows, row)

		if event.ChangeID != "" {
			changes[event.ChangeID] = true
		}
		actors[row.Actor] = true
		actionCounts[event.Action]++
		dayCounts[event.CreatedAt.In(cnZone).Format("2006-01-02")]++
		if i == 0 {
			data.FirstHash = shortHash(event.Hash)
		}
		data.LastHash = shortHash(event.Hash)
	}
	data.ChangeCount = len(changes)
	data.ActorCount = len(actors)
	data.ActiveDays = len(dayCounts)

	for name, count := range actionCounts {
		data.Actions = append(data.Actions, countRow{Name: nonEmpty(actionLabels[name], name), Count: count})
	}
	sort.Slice(data.Actions, func(i, j int) bool {
		if data.Actions[i].Count != data.Actions[j].Count {
			return data.Actions[i].Count > data.Actions[j].Count
		}
		return data.Actions[i].Name < data.Actions[j].Name
	})
	if len(data.Actions) > 8 {
		data.Actions = data.Actions[:8]
	}

	daysInMonth := end.Add(-time.Second).In(cnZone).Day()
	maxCount := 0
	for day := 1; day <= daysInMonth; day++ {
		key := fmtDay(start, day)
		if c := dayCounts[key]; c > maxCount {
			maxCount = c
		}
	}
	for day := 1; day <= daysInMonth; day++ {
		count := dayCounts[fmtDay(start, day)]
		height := 0
		if maxCount > 0 {
			height = count * 64 / maxCount
			if count > 0 && height < 4 {
				height = 4
			}
		}
		data.Days = append(data.Days, dayBar{Day: day, Count: count, Height: height})
	}
	return data
}

func fmtDay(monthStart time.Time, day int) string {
	return time.Date(monthStart.Year(), monthStart.Month(), day, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

func nonEmpty(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func shortHash(hash string) string {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return "—"
	}
	if len(hash) > 16 {
		return hash[:16] + "…"
	}
	return hash
}

func actorTag(actorType string) string {
	switch strings.ToUpper(actorType) {
	case "CI":
		return "CI"
	case "SYSTEM":
		return "系统"
	}
	return ""
}

func resultLabel(result string) string {
	switch strings.ToUpper(result) {
	case "SUCCESS", "":
		return "成功"
	case "FAILURE", "FAILED":
		return "失败"
	case "BLOCK", "BLOCKED":
		return "拦截"
	}
	return result
}

func roleLabel(role string) string {
	switch strings.ToUpper(role) {
	case "OWNER":
		return "技术负责人"
	case "DBA":
		return "DBA"
	case "DEVELOPER":
		return "研发"
	}
	return role
}

var monthlyFuncs = template.FuncMap{
	"mul": func(a, b int) int { return a * b },
	"div": func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a / b
	},
}

var monthlyTemplate = template.Must(template.New("monthly").Funcs(monthlyFuncs).Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>ChangeGuard 审计月报 · {{ .MonthLabel }} · {{ .Organization }}</title>
<style>
  :root { color-scheme: light; }
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body {
    font-family: "PingFang SC", "Microsoft YaHei UI", "Microsoft YaHei", "Noto Sans SC", "Segoe UI", sans-serif;
    color: #181c23; background: #f6f7f9; font-size: 12px; line-height: 1.5;
  }
  .sheet { max-width: 210mm; margin: 0 auto; background: #fff; padding: 14mm 12mm; }
  .toolbar {
    position: sticky; top: 0; display: flex; align-items: center; gap: 12px;
    padding: 10px 14px; margin-bottom: 14px; background: #eef0f4; border: 1px solid rgba(22,28,45,.14); border-radius: 6px;
  }
  .toolbar button {
    height: 30px; padding: 0 14px; border-radius: 6px; border: 1px solid #363f9e; background: #4a55cc; color: #fff;
    font: inherit; font-weight: 500; cursor: pointer;
  }
  .toolbar .hint { color: #5f6775; }
  header.report-head { border-bottom: 2px solid #181c23; padding-bottom: 14px; margin-bottom: 16px; }
  .brandline { display: flex; align-items: baseline; justify-content: space-between; gap: 12px; }
  h1 { font-size: 20px; font-weight: 650; letter-spacing: .01em; }
  .brand { font-family: Consolas, monospace; font-size: 10px; letter-spacing: .14em; color: #6e7684; }
  .meta { margin-top: 10px; display: grid; grid-template-columns: repeat(2, 1fr); gap: 4px 24px; font-size: 12px; }
  .meta b { font-weight: 500; color: #5f6775; margin-right: 6px; }
  h2 { font-size: 13px; font-weight: 600; margin: 18px 0 8px; padding-left: 8px; border-left: 3px solid #4a55cc; }
  .cards { display: grid; grid-template-columns: repeat(4, 1fr); gap: 8px; }
  .card { border: 1px solid rgba(22,28,45,.16); border-radius: 6px; padding: 10px 12px; }
  .card .k { font-size: 10px; color: #6e7684; letter-spacing: .08em; }
  .card .v { font-size: 20px; font-weight: 600; margin-top: 2px; }
  .split { display: grid; grid-template-columns: 1fr 1.4fr; gap: 16px; align-items: start; }
  table { width: 100%; border-collapse: collapse; }
  th { text-align: left; font-size: 10px; font-weight: 500; letter-spacing: .1em; color: #6e7684;
       border-bottom: 1px solid rgba(22,28,45,.25); padding: 5px 6px; }
  td { padding: 5px 6px; border-bottom: 1px solid rgba(22,28,45,.1); vertical-align: top; }
  .num { text-align: right; font-variant-numeric: tabular-nums; }
  .bars { display: flex; align-items: flex-end; gap: 2px; height: 74px; padding-top: 6px; }
  .bar { flex: 1; background: #4a55cc; border-radius: 1px 1px 0 0; min-height: 0; }
  .bar.zero { background: rgba(22,28,45,.12); height: 2px; }
  .bar-scale { position: relative; height: 14px; }
  .bar-day { position: absolute; transform: translateX(-50%); font-size: 8px; color: #6e7684; font-variant-numeric: tabular-nums; }
  .empty { border: 1px dashed rgba(22,28,45,.25); border-radius: 6px; padding: 24px; text-align: center; color: #6e7684; }
  .mono { font-family: Consolas, "JetBrains Mono", monospace; font-variant-numeric: tabular-nums; }
  .tag { display: inline-block; margin-left: 4px; padding: 0 4px; border: 1px solid rgba(22,28,45,.25); border-radius: 3px; font-size: 9px; color: #5f6775; }
  .mu { font-weight: 600; }
  .signoff { margin-top: 22px; border-top: 1px solid rgba(22,28,45,.2); padding-top: 12px; }
  .signoff td { height: 34px; border-bottom: 1px solid rgba(22,28,45,.2); }
  .footnote { margin-top: 10px; font-size: 10px; color: #6e7684; }
  @page { size: A4; margin: 12mm; }
  @media print {
    body { background: #fff; }
    .sheet { max-width: none; padding: 0; }
    .toolbar { display: none; }
    thead { display: table-header-group; }
    tr, .card, .split { break-inside: avoid; }
    h2 { break-after: avoid; }
  }
</style>
</head>
<body>
<div class="toolbar">
  <button type="button" onclick="window.print()">打印 / 另存为 PDF</button>
  <span class="hint">在打印对话框中选择"另存为 PDF"，即可归档提交合规部门。</span>
</div>
<div class="sheet">
  <header class="report-head">
    <div class="brandline">
      <h1>数据库变更审计月报 · {{ .MonthLabel }}</h1>
      <span class="brand">CHANGEGUARD · CHANGE RISK CONTROL</span>
    </div>
    <div class="meta">
      <span><b>组织</b>{{ .Organization }}</span>
      <span><b>统计期间</b>{{ .PeriodStart }} 至 {{ .PeriodEnd }}（UTC+8）</span>
      <span><b>生成时间</b>{{ .GeneratedAt }}</span>
      <span><b>生成人</b>{{ .GeneratedBy }}（{{ .GeneratedRole }}）</span>
    </div>
  </header>

  <h2>一、期间概览</h2>
  <div class="cards">
    <div class="card"><div class="k">审计事件</div><div class="v">{{ .Total }}</div></div>
    <div class="card"><div class="k">涉及变更单</div><div class="v">{{ .ChangeCount }}</div></div>
    <div class="card"><div class="k">操作人</div><div class="v">{{ .ActorCount }}</div></div>
    <div class="card"><div class="k">有操作天数</div><div class="v">{{ .ActiveDays }}</div></div>
  </div>

  {{ if .Empty }}
  <h2>二、明细</h2>
  <div class="empty">本统计期间内无审计事件。</div>
  {{ else }}
  <h2>二、按日分布与高频动作</h2>
  <div class="split">
    <div>
      <div class="bars">
        {{- range .Days }}
        <div class="bar {{ if eq .Count 0 }}zero{{ end }}" style="height: {{ .Height }}px" title="{{ .Day }} 日 {{ .Count }} 件"></div>
        {{- end }}
      </div>
      <div class="bar-scale">
        {{- range .Days }}
        {{- if or (eq .Day 1) (eq .Day 8) (eq .Day 15) (eq .Day 22) (eq .Day 29) }}
        <span class="bar-day" style="left: {{ div (mul .Day 100) (len $.Days) }}%">{{ .Day }}</span>
        {{- end }}
        {{- end }}
      </div>
    </div>
    <table>
      <thead><tr><th>动作</th><th class="num">次数</th></tr></thead>
      <tbody>
        {{- range .Actions }}
        <tr><td>{{ .Name }}</td><td class="num">{{ .Count }}</td></tr>
        {{- end }}
      </tbody>
    </table>
  </div>

  <h2>三、审计明细</h2>
  <table>
    <thead>
      <tr><th style="width:22px">#</th><th style="width:96px">时间</th><th style="width:110px">操作人</th>
          <th style="width:110px">动作</th><th style="width:88px">变更单</th><th>摘要</th><th style="width:40px">结果</th></tr>
    </thead>
    <tbody>
      {{- range .Rows }}
      <tr>
        <td class="num">{{ .Index }}</td>
        <td class="mono">{{ .Time }}</td>
        <td>{{ .Actor }}{{ if .ActorTag }}<span class="tag">{{ .ActorTag }}</span>{{ end }}</td>
        <td{{ if .ActionMu }} class="mu"{{ end }}>{{ .Action }}</td>
        <td class="mono">{{ .ChangeID }}</td>
        <td>{{ .Detail }}</td>
        <td>{{ .Result }}</td>
      </tr>
      {{- end }}
    </tbody>
  </table>
  {{ end }}

  <h2>四、存证与签署</h2>
  {{ if not .Empty }}
  <div class="meta" style="margin-bottom:6px">
    <span><b>起始事件哈希</b><span class="mono">{{ .FirstHash }}</span></span>
    <span><b>截止事件哈希</b><span class="mono">{{ .LastHash }}</span></span>
  </div>
  <p class="footnote">审计事件采用哈希链防篡改存证；完整链与原始事件可由系统管理员随时导出核验。本报告由系统按当前登录人可见范围生成。</p>
  {{ end }}
  <table class="signoff">
    <thead><tr><th>角色</th><th>签字</th><th style="width:140px">日期</th></tr></thead>
    <tbody>
      <tr><td>合规审核人</td><td></td><td></td></tr>
      <tr><td>数据库负责人</td><td></td><td></td></tr>
    </tbody>
  </table>
</div>
<script>
  window.addEventListener('load', function () {
    if (new URLSearchParams(location.search).get('print') === '1') {
      setTimeout(function () { window.print(); }, 350);
    }
  });
</script>
</body>
</html>
`))
