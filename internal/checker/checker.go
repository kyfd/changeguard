package checker

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

type Context struct {
	Environment  string
	ChangeType   string
	ArtifactKind model.ArtifactKind
}

type ReleaseInput struct {
	SQL          string
	RollbackSQL  string
	RollbackPlan string
	Artifacts    []model.ChangeArtifact
	ReleasePlan  model.ReleasePlan
}

type Result struct {
	Statements         []string        `json:"statements"`
	Findings           []model.Finding `json:"findings"`
	Risk               model.RiskLevel `json:"risk"`
	MatchedPolicyCodes []string        `json:"matched_policy_codes"`
}

func Check(sql, rollbackSQL string) Result {
	return CheckWithPolicies(sql, rollbackSQL, Context{}, model.DefaultRiskPolicies(time.Now()))
}

func CheckReleaseWithPolicies(input ReleaseInput, context Context, policies []model.RiskPolicy) Result {
	result := Result{Risk: model.RiskLow}
	appendFinding := func(policyCode, detail, evidence string, artifactKind model.ArtifactKind) {
		policy, ok := applicablePolicy(policies, policyCode, Context{Environment: context.Environment, ChangeType: context.ChangeType, ArtifactKind: artifactKind})
		if !ok {
			return
		}
		result.Findings = append(result.Findings, findingFromPolicy(policy, detail, evidence))
		result.MatchedPolicyCodes = append(result.MatchedPolicyCodes, policy.Code)
	}

	if strings.TrimSpace(input.SQL) != "" {
		sqlResult := CheckWithPolicies(input.SQL, input.RollbackSQL, Context{Environment: context.Environment, ChangeType: context.ChangeType, ArtifactKind: model.ArtifactDatabase}, policies)
		result.Statements = append(result.Statements, sqlResult.Statements...)
		result.Findings = append(result.Findings, sqlResult.Findings...)
		result.MatchedPolicyCodes = append(result.MatchedPolicyCodes, sqlResult.MatchedPolicyCodes...)
		result.Risk = maxRisk(result.Risk, sqlResult.Risk)
	}

	if strings.TrimSpace(input.RollbackPlan) == "" && strings.TrimSpace(input.RollbackSQL) == "" {
		appendFinding("MISSING_RELEASE_ROLLBACK", "当前变更没有可执行的版本回退、配置恢复或数据补偿步骤。", "rollback_plan 为空", "")
	}

	for _, artifact := range input.Artifacts {
		content := strings.TrimSpace(artifact.Content)
		if content == "" {
			continue
		}
		switch artifact.Kind {
		case model.ArtifactKubernetes:
			for _, detection := range changegate.CheckKubernetes(content, artifact.Name, changegate.IsProduction(context.Environment)) {
				appendFinding(detection.Code, detection.Detail, detection.Evidence, artifact.Kind)
			}
		case model.ArtifactConfig:
			for _, detection := range changegate.CheckConfig(content, artifact.Name, changegate.IsProduction(context.Environment)) {
				appendFinding(detection.Code, detection.Detail, detection.Evidence, artifact.Kind)
			}
		}
		for _, policy := range policies {
			if strings.TrimSpace(policy.Pattern) == "" || len(policy.ArtifactKinds) == 0 || !policyApplies(policy, Context{Environment: context.Environment, ChangeType: context.ChangeType, ArtifactKind: artifact.Kind}) {
				continue
			}
			pattern, err := regexp.Compile(policy.Pattern)
			if err == nil && pattern.MatchString(content) {
				result.Findings = append(result.Findings, findingFromPolicy(policy, policy.Description, compactMatch(artifact, "custom policy match")))
				result.MatchedPolicyCodes = append(result.MatchedPolicyCodes, policy.Code)
			}
		}
	}

	production := strings.Contains(strings.ToLower(context.Environment), "prod") || strings.Contains(context.Environment, "生产")
	strategy := strings.TrimSpace(input.ReleasePlan.Strategy)
	if production && (strategy == "" || strategy == "全量发布") {
		appendFinding("PRODUCTION_FULL_RELEASE", "生产环境计划直接全量发布，故障影响半径较大。", "strategy="+defaultValue(strategy, "全量发布"), "")
	}
	if len(input.ReleasePlan.SuccessMetrics) == 0 {
		appendFinding("MISSING_OBSERVATION_METRICS", "发布计划没有配置错误率、延迟或业务成功指标。", "success_metrics=0", "")
	}

	for _, finding := range result.Findings {
		result.Risk = maxRisk(result.Risk, finding.Severity)
	}
	result.MatchedPolicyCodes = uniqueCodes(result.MatchedPolicyCodes)
	return result
}

var (
	foreignKeyPattern       = regexp.MustCompile(`(?is)\bFOREIGN\s+KEY\b|\bREFERENCES\s+[a-zA-Z_][\w$]*`)
	addConstraintPattern    = regexp.MustCompile(`(?is)\bADD\s+(CONSTRAINT\s+[a-zA-Z_][\w$]*\s+)?FOREIGN\s+KEY\b|\bADD\s+CONSTRAINT\s+[a-zA-Z_][\w$]*\s+FOREIGN\s+KEY\b`)
	notValidPattern         = regexp.MustCompile(`(?is)\bNOT\s+VALID\b`)
	forUpdatePattern        = regexp.MustCompile(`(?is)\bFOR\s+UPDATE(\s+OF\s+[a-zA-Z_][\w$]*)?(\s+NOWAIT|\s+SKIP\s+LOCKED)?\b`)
	limitPattern            = regexp.MustCompile(`(?is)\bLIMIT\s+\d+\b`)
	batchHintPattern        = regexp.MustCompile(`(?is)\b(BATCH|CHUNK|ROWNUM|FETCH\s+FIRST\s+\d+\s+ROWS\s+ONLY)\b`)
	heavyDDLPattern         = regexp.MustCompile(`(?is)\b(VACUUM\s+FULL|CLUSTER\s+[a-zA-Z_][\w$]*|REINDEX\s+(TABLE|INDEX|DATABASE|SYSTEM)\b|ALTER\s+TABLE[\s\S]*ALTER\s+COLUMN[\s\S]*TYPE\b)`)
	lockTimeoutPattern      = regexp.MustCompile(`(?is)\b(SET(\s+LOCAL)?\s+lock_timeout\b|lock_timeout\s*=)`)
	statementTimeoutPattern = regexp.MustCompile(`(?is)\b(SET(\s+LOCAL)?\s+statement_timeout\b|statement_timeout\s*=)`)
	ddlVerbPattern          = regexp.MustCompile(`(?is)^\s*(CREATE|ALTER|DROP|REINDEX|CLUSTER|VACUUM|TRUNCATE)\b`)
	dmlVerbPattern          = regexp.MustCompile(`(?is)^\s*(UPDATE|DELETE|INSERT|MERGE)\b`)
	whereClausePattern      = regexp.MustCompile(`(?is)\bWHERE\b`)
)

func CheckWithPolicies(sql, rollbackSQL string, context Context, policies []model.RiskPolicy) Result {
	statements := SplitStatements(sql)
	result := Result{Statements: statements, Risk: model.RiskLow}
	active := make(map[string]model.RiskPolicy, len(policies))
	type compiledPolicy struct {
		policy  model.RiskPolicy
		pattern *regexp.Regexp
	}
	compiled := make([]compiledPolicy, 0, len(policies))
	for _, policy := range policies {
		if !policy.Enabled || !policyApplies(policy, context) {
			continue
		}
		active[policy.Code] = policy
		if strings.TrimSpace(policy.Pattern) == "" {
			continue
		}
		pattern, err := regexp.Compile(policy.Pattern)
		if err == nil {
			compiled = append(compiled, compiledPolicy{policy: policy, pattern: pattern})
		}
	}
	add := func(code, detail, evidence string) {
		policy, ok := active[code]
		if !ok {
			return
		}
		result.Findings = append(result.Findings, findingFromPolicy(policy, detail, evidence))
		result.MatchedPolicyCodes = append(result.MatchedPolicyCodes, policy.Code)
	}

	if strings.TrimSpace(sql) == "" {
		add("EMPTY_SQL", "变更单没有可执行语句。", "")
	}
	if strings.TrimSpace(rollbackSQL) == "" {
		add("MISSING_ROLLBACK", "发生异常时缺少明确恢复步骤。", "rollback_sql 为空")
	}
	if len(statements) > 8 {
		add("TOO_MANY_STATEMENTS", fmt.Sprintf("当前包含 %d 条 SQL，失败时定位和回滚成本较高。", len(statements)), fmt.Sprintf("statements=%d", len(statements)))
	}

	hasDDL, hasBulkDML, needsLockTimeout := false, false, false
	combinedSQL := sql + "\n" + rollbackSQL

	for _, statement := range statements {
		upper := strings.ToUpper(statement)
		trimmedUpper := strings.TrimSpace(upper)
		isUpdate := strings.HasPrefix(trimmedUpper, "UPDATE ")
		isDelete := strings.HasPrefix(trimmedUpper, "DELETE FROM ") || strings.HasPrefix(trimmedUpper, "DELETE ")
		hasWhere := whereClausePattern.MatchString(statement)
		hasBatchBound := limitPattern.MatchString(statement) || batchHintPattern.MatchString(statement)

		if isUpdate && !hasWhere {
			add("UPDATE_WITHOUT_WHERE", "语句可能更新整张表。", compact(statement))
		}
		if isDelete && !hasWhere {
			add("DELETE_WITHOUT_WHERE", "语句可能删除整张表的数据。", compact(statement))
		}
		// Bounded DML still needs batching when it can touch many rows in one TX.
		if (isUpdate || isDelete) && hasWhere && !hasBatchBound {
			add("UNBATCHED_LARGE_DML", "条件 DML 缺少 LIMIT/分批边界，可能在单事务中锁住大量行并拉长 WAL/回滚窗口。", compact(statement))
			hasBulkDML = true
			needsLockTimeout = true
		}
		if (isUpdate || isDelete) && hasBatchBound {
			hasBulkDML = true
			needsLockTimeout = true
		}
		if strings.Contains(upper, " ADD ") && strings.Contains(upper, " NOT NULL") && !strings.Contains(upper, " DEFAULT ") {
			add("ADD_NOT_NULL_WITHOUT_DEFAULT", "历史数据无法自动满足非空约束，迁移可能直接失败。", compact(statement))
			needsLockTimeout = true
		}
		if (strings.Contains(upper, "CREATE INDEX ") || strings.Contains(upper, "CREATE UNIQUE INDEX ")) && !strings.Contains(upper, "INDEX CONCURRENTLY ") {
			add("INDEX_NOT_CONCURRENT", "高写入表直接创建索引可能扩大锁等待窗口。", compact(statement))
			needsLockTimeout = true
			hasDDL = true
		}
		// Foreign keys without NOT VALID force a full table scan under lock.
		if (addConstraintPattern.MatchString(statement) || (strings.Contains(upper, "ADD CONSTRAINT") && foreignKeyPattern.MatchString(statement)) || (strings.Contains(upper, "FOREIGN KEY") && strings.Contains(upper, "REFERENCES"))) && !notValidPattern.MatchString(statement) {
			add("FK_WITHOUT_NOT_VALID", "直接添加外键会在创建时整表校验并持有较长时间锁。", compact(statement))
			needsLockTimeout = true
			hasDDL = true
		}
		if forUpdatePattern.MatchString(statement) && !limitPattern.MatchString(statement) && !hasPrimaryKeyPredicate(upper) {
			add("SELECT_FOR_UPDATE_UNBOUNDED", "FOR UPDATE 缺少 LIMIT 或明确主键范围，可能锁住过量行。", compact(statement))
			needsLockTimeout = true
		}
		if heavyDDLPattern.MatchString(statement) {
			add("HEAVY_DDL_REWRITE", "重写型 DDL 可能触发表重写与长事务，阻塞读写。", compact(statement))
			needsLockTimeout = true
			hasDDL = true
		}
		if ddlVerbPattern.MatchString(statement) {
			hasDDL = true
			if strings.Contains(upper, "ALTER TABLE") || strings.Contains(upper, "DROP INDEX") || strings.Contains(upper, "CREATE INDEX") {
				needsLockTimeout = true
			}
		}
		if dmlVerbPattern.MatchString(statement) && (isUpdate || isDelete) {
			// already counted bulk DML above when applicable
		}

		for _, item := range compiled {
			if !item.pattern.MatchString(statement) {
				continue
			}
			result.Findings = append(result.Findings, findingFromPolicy(item.policy, item.policy.Description, compact(statement)))
			result.MatchedPolicyCodes = append(result.MatchedPolicyCodes, item.policy.Code)
		}
	}

	if hasDDL && hasBulkDML {
		add("MIXED_DDL_DML_TRANSACTION", "同一变更同时包含 DDL 与大批量 DML，事务边界与回滚成本难以控制。", fmt.Sprintf("statements=%d", len(statements)))
	}
	if needsLockTimeout && !lockTimeoutPattern.MatchString(combinedSQL) && !statementTimeoutPattern.MatchString(combinedSQL) {
		add("MISSING_LOCK_TIMEOUT", "高锁风险语句未声明 lock_timeout/statement_timeout，锁冲突时可能长时间阻塞业务。", "lock_timeout 未声明")
	}

	for _, item := range result.Findings {
		result.Risk = maxRisk(result.Risk, item.Severity)
	}
	return result
}

// hasPrimaryKeyPredicate is a conservative heuristic: equality on id/pk-like
// columns reduces the chance of unbounded row locks.
func hasPrimaryKeyPredicate(upper string) bool {
	markers := []string{" ID =", " ID=", ".ID =", ".ID=", " PK =", " PK=", "_ID =", "_ID=", " PRIMARY KEY"}
	for _, marker := range markers {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func policyApplies(policy model.RiskPolicy, context Context) bool {
	if len(policy.ArtifactKinds) > 0 && !containsFold(policy.ArtifactKinds, string(context.ArtifactKind)) {
		return false
	}
	if len(policy.Environments) > 0 && !containsFold(policy.Environments, context.Environment) {
		return false
	}
	if len(policy.ChangeTypes) > 0 && !containsFold(policy.ChangeTypes, context.ChangeType) {
		return false
	}
	return true
}

func applicablePolicy(policies []model.RiskPolicy, code string, context Context) (model.RiskPolicy, bool) {
	for _, policy := range policies {
		if policy.Code == code && policy.Enabled && policyApplies(policy, context) {
			return policy, true
		}
	}
	return model.RiskPolicy{}, false
}

func compactMatch(artifact model.ChangeArtifact, match string) string {
	name := strings.TrimSpace(artifact.Name)
	if name == "" {
		name = string(artifact.Kind)
	}
	return compact(fmt.Sprintf("%s: %s", name, match))
}

func defaultValue(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

func uniqueCodes(items []string) []string {
	seen := make(map[string]bool, len(items))
	result := make([]string, 0, len(items))
	for _, item := range items {
		if item != "" && !seen[item] {
			seen[item] = true
			result = append(result, item)
		}
	}
	return result
}

func containsFold(items []string, value string) bool {
	for _, item := range items {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

func findingFromPolicy(policy model.RiskPolicy, detail, evidence string) model.Finding {
	now := time.Now()
	if strings.TrimSpace(detail) == "" {
		detail = policy.Description
	}
	return model.Finding{
		ID: store.NewID("ev_rule_"), Code: policy.Code, Severity: policy.Severity,
		Title: policy.Name, Detail: detail, Evidence: evidence, Suggestion: policy.Suggestion,
		Blocking: policy.Blocking, RuleVersion: policy.Version, Status: model.FindingOpen, UpdatedAt: now,
	}
}

func SplitStatements(input string) []string {
	var statements []string
	var buf strings.Builder
	var single, double, lineComment, blockComment bool
	for i := 0; i < len(input); i++ {
		ch := input[i]
		var next byte
		if i+1 < len(input) {
			next = input[i+1]
		}
		if lineComment {
			buf.WriteByte(ch)
			if ch == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			buf.WriteByte(ch)
			if ch == '*' && next == '/' {
				buf.WriteByte(next)
				i++
				blockComment = false
			}
			continue
		}
		if !single && !double && ch == '-' && next == '-' {
			lineComment = true
			buf.WriteByte(ch)
			buf.WriteByte(next)
			i++
			continue
		}
		if !single && !double && ch == '/' && next == '*' {
			blockComment = true
			buf.WriteByte(ch)
			buf.WriteByte(next)
			i++
			continue
		}
		if ch == '\'' && !double {
			if single && next == '\'' {
				buf.WriteByte(ch)
				buf.WriteByte(next)
				i++
				continue
			}
			single = !single
		}
		if ch == '"' && !single {
			double = !double
		}
		if ch == ';' && !single && !double {
			if value := strings.TrimSpace(buf.String()); value != "" {
				statements = append(statements, value+";")
			}
			buf.Reset()
			continue
		}
		buf.WriteByte(ch)
	}
	if value := strings.TrimSpace(buf.String()); value != "" {
		statements = append(statements, value)
	}
	return statements
}

func maxRisk(left, right model.RiskLevel) model.RiskLevel {
	rank := map[model.RiskLevel]int{model.RiskUnknown: 0, model.RiskLow: 1, model.RiskMedium: 2, model.RiskHigh: 3}
	if rank[right] > rank[left] {
		return right
	}
	return left
}

func compact(value string) string {
	value = changegate.Redact(value)
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > 180 {
		return value[:177] + "..."
	}
	return value
}
