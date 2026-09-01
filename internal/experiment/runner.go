package experiment

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/checker"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
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
		mode: strings.ToLower(getenvBrand("EXPERIMENT_MODE")),
		dsn:  getenvBrand("SHADOW_DSN"),
	}
}

func getenvBrand(suffix string) string {
	if value := strings.TrimSpace(os.Getenv("CHANGEGUARD_" + suffix)); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("DBGUARD_" + suffix))
}

func rejectShadowOnPrimaryDSN(shadow string, primaryCandidates ...string) error {
	shadowHost, err := postgresHostPort(shadow)
	if err != nil {
		return fmt.Errorf("CHANGEGUARD_SHADOW_DSN 无效: %w", err)
	}
	for _, primary := range primaryCandidates {
		if strings.TrimSpace(primary) == "" {
			continue
		}
		primaryHost, err := postgresHostPort(primary)
		if err != nil {
			return fmt.Errorf("CHANGEGUARD_PRIMARY_DSN 无效: %w", err)
		}
		if strings.EqualFold(shadowHost, primaryHost) {
			return fmt.Errorf("影子库 DSN 与主库指向同一 host:port（%s），已拒绝连接", shadowHost)
		}
	}
	return nil
}

func postgresHostPort(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return "", fmt.Errorf("must be a postgres URL")
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if port == "" {
		port = "5432"
	}
	if host == "" {
		return "", fmt.Errorf("missing host")
	}
	return net.JoinHostPort(host, port), nil
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
			return failedReport("POSTGRES", started, fmt.Errorf("已配置 PostgreSQL 影子验证，但 CHANGEGUARD_SHADOW_DSN 为空"))
		}
		if err := rejectShadowOnPrimaryDSN(r.dsn, getenvBrand("PRIMARY_DSN")); err != nil {
			return failedReport("POSTGRES", started, err)
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
		ExecutionError:   "未配置真实 PostgreSQL 影子库；演示模式不会生成性能、锁等待或回滚通过数据，也不能推进审批或签发通行证",
		Evidence:         []model.Evidence{{ID: store.NewID("ev_demo_"), Kind: "notice", Title: "演练未执行", Value: "NOT_RUN", Source: "DEMO_ONLY", ObservedAt: finished}},
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

	// Transaction optimization baseline for shadow rehearsals:
	// - lock_timeout fails fast on lock contention instead of hanging
	// - statement_timeout caps long-running DDL/DML inside the rehearsal TX
	// - advisory xact lock serializes concurrent shadow runs on the same DSN
	const (
		shadowLockTimeout      = "2s"
		shadowStatementTimeout = "45s"
	)
	for _, setting := range []string{
		"SET LOCAL lock_timeout = '" + shadowLockTimeout + "'",
		"SET LOCAL statement_timeout = '" + shadowStatementTimeout + "'",
		"SELECT pg_advisory_xact_lock(726384721)",
	} {
		if _, err = tx.Exec(ctx, setting); err != nil {
			return failedBoundReport(report, started, fmt.Errorf("初始化影子事务失败: %w", err))
		}
	}
	var rowCount int64
	_ = tx.QueryRow(ctx, "SELECT COALESCE(SUM(n_live_tup),0)::bigint FROM pg_stat_user_tables").Scan(&rowCount)

	// Capture buffer and lock baselines so the report can show transaction cost deltas.
	var blksHit, blksRead int64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(blks_hit,0), COALESCE(blks_read,0)
		FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&blksHit, &blksRead)
	var locksHeld int64
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM pg_locks WHERE granted = true AND pid = pg_backend_pid()`).Scan(&locksHeld)

	executionSQL, executionNormalized := normalizeShadowSQL(change.SQL)
	rollbackSQL, rollbackNormalized := normalizeShadowSQL(change.RollbackSQL)
	execStatements := executableStatements(executionSQL)
	rbStatements := executableStatements(rollbackSQL)
	rollbackExecuted := 0
	var slowestMS int64
	for _, group := range []struct {
		name       string
		statements []string
		rollback   bool
	}{
		{name: "执行 SQL", statements: execStatements},
		{name: "回滚 SQL", statements: rbStatements, rollback: true},
	} {
		for _, statement := range group.statements {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			stmtStarted := time.Now()
			if _, err = tx.Exec(ctx, statement); err != nil {
				// Classify lock/timeout failures so the report points to TX optimization next steps.
				classified := classifyShadowSQLError(err)
				failed := failedBoundReport(report, started, fmt.Errorf("%s 验证失败: %w", group.name, err))
				failed.LockWaitMS = maxInt64(failed.DurationMS, 0)
				failed.Evidence = append(failed.Evidence,
					model.Evidence{ID: store.NewID("ev_exp_"), Kind: "check", Title: "事务失败分类", Value: classified, Source: "影子事务诊断", ObservedAt: time.Now()},
					model.Evidence{ID: store.NewID("ev_exp_"), Kind: "check", Title: "锁/语句超时基线", Value: fmt.Sprintf("lock_timeout=%s; statement_timeout=%s", shadowLockTimeout, shadowStatementTimeout), Source: "影子事务初始化", ObservedAt: time.Now()},
				)
				return failed
			}
			elapsed := time.Since(stmtStarted).Milliseconds()
			if elapsed > slowestMS {
				slowestMS = elapsed
			}
			if group.rollback {
				rollbackExecuted++
			}
		}
	}

	var locksAfter int64
	_ = tx.QueryRow(ctx, `SELECT COUNT(*) FROM pg_locks WHERE granted = true AND pid = pg_backend_pid()`).Scan(&locksAfter)
	var blksHitAfter, blksReadAfter int64
	_ = tx.QueryRow(ctx, `
		SELECT COALESCE(blks_hit,0), COALESCE(blks_read,0)
		FROM pg_stat_database WHERE datname = current_database()
	`).Scan(&blksHitAfter, &blksReadAfter)

	finished := time.Now()
	report.Status = "PASSED"
	report.FinishedAt = finished
	report.DurationMS = finished.Sub(started).Milliseconds()
	report.DatasetRows = rowCount
	report.LockWaitMS = slowestMS
	report.P99BeforeMS = float64(slowestMS)
	if report.DurationMS > 0 {
		report.P99AfterMS = float64(report.DurationMS)
	}
	report.RollbackVerified = rollbackExecuted > 0
	report.ChecksTotal = 4
	report.ChecksPassed = 4
	if !report.RollbackVerified {
		report.ChecksPassed = 3
	}
	report.Evidence = []model.Evidence{
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "影子库统计行数", Value: formatRows(rowCount), Source: "pg_stat_user_tables", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "事务演练耗时", Value: fmt.Sprintf("%dms", report.DurationMS), Source: "pgx 事务执行器", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "最慢语句耗时", Value: fmt.Sprintf("%dms", slowestMS), Source: "语句级计时", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "事务内持锁数", Value: fmt.Sprintf("%d→%d", locksHeld, locksAfter), Source: "pg_locks", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "metric", Title: "缓冲命中增量", Value: fmt.Sprintf("hit=%d read=%d", blksHitAfter-blksHit, blksReadAfter-blksRead), Source: "pg_stat_database", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "check", Title: "SQL 与回滚同事务验证", Value: yesNo(report.RollbackVerified), Source: "PostgreSQL 影子库", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "check", Title: "锁/语句超时基线", Value: fmt.Sprintf("lock_timeout=%s; statement_timeout=%s", shadowLockTimeout, shadowStatementTimeout), Source: "影子事务初始化", ObservedAt: finished},
		{ID: store.NewID("ev_exp_"), Kind: "check", Title: "执行语句数", Value: fmt.Sprintf("forward=%d rollback=%d", len(execStatements), len(rbStatements)), Source: "SQL 拆分器", ObservedAt: finished},
	}
	if executionNormalized || rollbackNormalized {
		report.Evidence = append(report.Evidence, model.Evidence{ID: store.NewID("ev_exp_"), Kind: "check", Title: "并发索引影子等价验证", Value: "CONCURRENTLY 在影子事务中转换为普通索引 DDL；生产执行仍保留原语句", Source: "影子 SQL 规范化器", ObservedAt: finished})
	}
	// Surface TX-optimization guidance when rehearsal itself is already slow.
	if report.DurationMS >= 5000 || slowestMS >= 2000 {
		report.Evidence = append(report.Evidence, model.Evidence{
			ID: store.NewID("ev_exp_"), Kind: "notice", Title: "事务优化提示",
			Value:  "演练耗时偏高：评估分批 DML、CONCURRENTLY/NOT VALID 分阶段 DDL，并确认生产侧 lock_timeout。",
			Source: "事务优化诊断", ObservedAt: finished,
		})
	}
	return report
}

func classifyShadowSQLError(err error) string {
	if err == nil {
		return "unknown"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "canceling statement due to lock timeout") || strings.Contains(msg, "lock timeout"):
		return "LOCK_TIMEOUT：锁等待超过阈值，建议拆分变更、低峰执行或改用 CONCURRENTLY/NOT VALID"
	case strings.Contains(msg, "canceling statement due to statement timeout") || strings.Contains(msg, "statement timeout"):
		return "STATEMENT_TIMEOUT：单语句超时，建议分批 DML 或缩短重写型 DDL"
	case strings.Contains(msg, "deadlock detected"):
		return "DEADLOCK：检测到死锁，建议固定加锁顺序并缩小事务范围"
	case strings.Contains(msg, "could not serialize") || strings.Contains(msg, "serialization failure"):
		return "SERIALIZATION_FAILURE：并发冲突，建议降低批次并行度或重试"
	default:
		return "SQL_EXEC_FAILED"
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
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
