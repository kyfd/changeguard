package experiment

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/liufengxi/dbguard/internal/changegate"
	"github.com/liufengxi/dbguard/internal/checker"
	"github.com/liufengxi/dbguard/internal/model"
	"github.com/liufengxi/dbguard/internal/store"
)

type Runner interface {
	Run(context.Context, model.ChangeRequest) model.ExperimentReport
}

type HybridRunner struct {
	mode string
	dsn  string
}

func NewFromEnvironment() Runner {
	return &HybridRunner{
		mode: strings.ToLower(strings.TrimSpace(os.Getenv("DBGUARD_EXPERIMENT_MODE"))),
		dsn:  strings.TrimSpace(os.Getenv("DBGUARD_SHADOW_DSN")),
	}
}

func (r *HybridRunner) Run(ctx context.Context, change model.ChangeRequest) model.ExperimentReport {
	started := time.Now()
	if err := validateExperimentSQL(change.SQL, change.RollbackSQL); err != nil {
		return failedReport(strings.ToUpper(defaultMode(r.mode)), started, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if r.mode == "postgres" {
		if r.dsn == "" {
			return failedReport("POSTGRES", started, fmt.Errorf("已配置 PostgreSQL 影子验证，但 DBGUARD_SHADOW_DSN 为空"))
		}
		return runPostgres(runCtx, r.dsn, change)
	}
	return runSimulation(runCtx, change)
}

func runSimulation(ctx context.Context, change model.ChangeRequest) model.ExperimentReport {
	started := time.Now()
	if err := ctx.Err(); err != nil {
		return failedReport("DEMO_ONLY", started, err)
	}
	finished := time.Now()
	return model.ExperimentReport{
		ID: store.NewID("exp_demo_"), Kind: "SQL_SHADOW_EXPERIMENT", Mode: "DEMO_ONLY", Status: "NOT_RUN",
		StartedAt: started, FinishedAt: finished, DurationMS: finished.Sub(started).Milliseconds(),
		RollbackVerified: false,
		ExecutionError: "未配置真实 PostgreSQL 影子库；演示模式不会生成性能、锁等待或回滚通过数据，也不能推进审批或签发通行证",
		Evidence: []model.Evidence{{ID: store.NewID("ev_demo_"), Kind: "notice", Title: "演练未执行", Value: "NOT_RUN", Source: "DEMO_ONLY", ObservedAt: finished}},
	}
}
func runPostgres(ctx context.Context, dsn string, change model.ChangeRequest) model.ExperimentReport {
	started := time.Now()
	report := model.ExperimentReport{
		ID: store.NewID("exp_"), Kind: "DATABASE_SHADOW_EXPERIMENT", Mode: "POSTGRES", Status: "FAILED", StartedAt: started,
		ArtifactSHA256: change.ArtifactSHA256, RuleSetVersion: change.RuleSetVersion,
	}
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return failedBoundReport(report, started, fmt.Errorf("连接 PostgreSQL 影子库失败: %w", err))
	}
	defer func() { _ = conn.Close(context.Background()) }()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return failedBoundReport(report, started, fmt.Errorf("开启影子事务失败: %w", err))
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, setting := range []string{
		"SET LOCAL lock_timeout = '2s'",
		"SET LOCAL statement_timeout = '45s'",
		"SELECT pg_advisory_xact_lock(726384721)",
	} {
		if _, err = tx.Exec(ctx, setting); err != nil {
			return failedBoundReport(report, started, fmt.Errorf("初始化影子事务失败: %w", err))
		}
	}
	var rowCount int64
	_ = tx.QueryRow(ctx, "SELECT COALESCE(SUM(n_live_tup),0)::bigint FROM pg_stat_user_tables").Scan(&rowCount)

	executionSQL, executionNormalized := normalizeShadowSQL(change.SQL)
	rollbackSQL, rollbackNormalized := normalizeShadowSQL(change.RollbackSQL)
	rollbackExecuted := 0
	for _, group := range []struct {
		name       string
		statements []string
		rollback   bool
	}{
		{name: "执行 SQL", statements: executableStatements(executionSQL)},
		{name: "回滚 SQL", statements: executableStatements(rollbackSQL), rollback: true},
	} {
		for _, statement := range group.statements {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if _, err = tx.Exec(ctx, statement); err != nil {
				return failedBoundReport(report, started, fmt.Errorf("%s 验证失败: %w", group.name, err))
			}
			if group.rollback {
				rollbackExecuted++
			}
		}
	}
	finished := time.Now()
	report.Status = "PASSED"
	report.FinishedAt = finished
	report.DurationMS = finished.Sub(started).Milliseconds()
	report.DatasetRows = rowCount
	report.RollbackVerified = rollbackExecuted > 0
	report.Evidence = []model.Evidence{
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "影子库统计行数", Value: formatRows(rowCount), Source: "pg_stat_user_tables", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "事务演练耗时", Value: fmt.Sprintf("%dms", report.DurationMS), Source: "pgx 事务执行器", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "check", Title: "SQL 与回滚同事务验证", Value: yesNo(report.RollbackVerified), Source: "PostgreSQL 影子库", ObservedAt: finished},
	}
	if executionNormalized || rollbackNormalized {
		report.Evidence = append(report.Evidence, model.Evidence{ID: store.NewID("ev_exp_"), Kind: "check", Title: "并发索引影子等价验证", Value: "CONCURRENTLY 在影子事务中转换为普通索引 DDL；生产执行仍保留原语句", Source: "影子 SQL 规范化器", ObservedAt: finished})
	}
	return report
}

func failedBoundReport(report model.ExperimentReport, started time.Time, err error) model.ExperimentReport {
	finished := time.Now()
	report.Status = "FAILED"
	report.FinishedAt = finished
	report.DurationMS = finished.Sub(started).Milliseconds()
	report.FailedTransactions = 1
	report.RollbackVerified = false
	report.ExecutionError = changegate.Redact(err.Error())
	return report
}

var concurrentIndexPattern = regexp.MustCompile(`(?i)\b(CREATE\s+(UNIQUE\s+)?INDEX|DROP\s+INDEX)\s+CONCURRENTLY\b`)

func normalizeShadowSQL(input string) (string, bool) {
	output := concurrentIndexPattern.ReplaceAllString(input, "$1")
	return output, output != input
}

func executableStatements(input string) []string {
	statements := checker.SplitStatements(input)
	result := make([]string, 0, len(statements))
	for _, statement := range statements {
		normalized := strings.TrimSpace(strings.TrimSuffix(stripLeadingSQLComments(statement), ";"))
		if normalized != "" {
			result = append(result, statement)
		}
	}
	return result
}

func validateExperimentSQL(sql, rollbackSQL string) error {
	for _, item := range []struct {
		name string
		sql  string
	}{{name: "执行 SQL", sql: sql}, {name: "回滚 SQL", sql: rollbackSQL}} {
		if containsPSQLMetaCommand(item.sql) {
			return fmt.Errorf("%s 包含 psql 元命令，影子演练已拒绝执行", item.name)
		}
		for _, statement := range checker.SplitStatements(item.sql) {
			normalized := strings.ToUpper(strings.TrimSpace(strings.TrimSuffix(stripLeadingSQLComments(statement), ";")))
			for _, command := range []string{
				"BEGIN", "START TRANSACTION", "COMMIT", "END", "ROLLBACK", "SAVEPOINT",
				"RELEASE SAVEPOINT", "PREPARE TRANSACTION", "COMMIT PREPARED", "ROLLBACK PREPARED",
			} {
				if normalized == command || strings.HasPrefix(normalized, command+" ") {
					return fmt.Errorf("%s 不允许包含事务控制语句 %s", item.name, command)
				}
			}
		}
	}
	return nil
}

func containsPSQLMetaCommand(input string) bool {
	var single, double, lineComment, blockComment bool
	for index := 0; index < len(input); index++ {
		current := input[index]
		var next byte
		if index+1 < len(input) {
			next = input[index+1]
		}
		if lineComment {
			if current == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if current == '*' && next == '/' {
				index++
				blockComment = false
			}
			continue
		}
		if !single && !double && current == '-' && next == '-' {
			index++
			lineComment = true
			continue
		}
		if !single && !double && current == '/' && next == '*' {
			index++
			blockComment = true
			continue
		}
		if current == '\'' && !double {
			if single && next == '\'' {
				index++
				continue
			}
			single = !single
			continue
		}
		if current == '"' && !single {
			double = !double
			continue
		}
		if current == '\\' && !single && !double {
			return true
		}
	}
	return false
}

func stripLeadingSQLComments(input string) string {
	value := strings.TrimSpace(input)
	for {
		switch {
		case strings.HasPrefix(value, "--"):
			if end := strings.IndexByte(value, '\n'); end >= 0 {
				value = strings.TrimSpace(value[end+1:])
				continue
			}
			return ""
		case strings.HasPrefix(value, "/*"):
			if end := strings.Index(value[2:], "*/"); end >= 0 {
				value = strings.TrimSpace(value[end+4:])
				continue
			}
			return ""
		default:
			return value
		}
	}
}

func defaultMode(mode string) string {
	if strings.TrimSpace(mode) == "" {
		return "demo_only"
	}
	return mode
}

func failedReport(mode string, started time.Time, err error) model.ExperimentReport {
	finished := time.Now()
	return model.ExperimentReport{
		ID: store.NewID("exp_"), Mode: mode, Status: "FAILED", StartedAt: started, FinishedAt: finished,
		DurationMS: finished.Sub(started).Milliseconds(), FailedTransactions: 1, ExecutionError: changegate.Redact(err.Error()),
	}
}

func formatRows(value int64) string {
	if value >= 1000000 {
		return fmt.Sprintf("%.2fM", float64(value)/1000000)
	}
	if value >= 1000 {
		return fmt.Sprintf("%.1fK", float64(value)/1000)
	}
	return strconv.FormatInt(value, 10)
}

func yesNo(value bool) string {
	if value {
		return "通过"
	}
	return "未通过"
}
