package checker

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kyfd/changeguard/internal/changegate"
	"github.com/kyfd/changeguard/internal/model"
	"github.com/kyfd/changeguard/internal/store"
)

type cachedPolicyPattern struct {
	re  *regexp.Regexp
	err error
}

var policyPatternCache sync.Map

func compilePolicyPattern(expr string) (*regexp.Regexp, error) {
	if cached, ok := policyPatternCache.Load(expr); ok {
		entry := cached.(cachedPolicyPattern)
		return entry.re, entry.err
	}
	re, err := regexp.Compile(expr)
	actual, _ := policyPatternCache.LoadOrStore(expr, cachedPolicyPattern{re: re, err: err})
	entry := actual.(cachedPolicyPattern)
	return entry.re, entry.err
}

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
			pattern, err := compilePolicyPattern(policy.Pattern)
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
	defaultKeywordPattern   = regexp.MustCompile(`(?is)\bDEFAULT\b`)
	createIndexPattern      = regexp.MustCompile(`(?is)\bCREATE\s+(UNIQUE\s+)?INDEX\b`)
	concurrentIndexPattern  = regexp.MustCompile(`(?is)\bCREATE\s+(UNIQUE\s+)?INDEX\s+CONCURRENTLY\b`)
	forUpdatePattern        = regexp.MustCompile(`(?is)\bFOR\s+UPDATE(\s+OF\s+[a-zA-Z_][\w$]*)?(\s+NOWAIT|\s+SKIP\s+LOCKED)?\b`)
	limitPattern            = regexp.MustCompile(`(?is)\bLIMIT\s+\d+\b`)
	batchHintPattern        = regexp.MustCompile(`(?is)\b(BATCH|CHUNK|ROWNUM|FETCH\s+FIRST\s+\d+\s+ROWS\s+ONLY)\b`)
	heavyDDLPattern         = regexp.MustCompile(`(?is)\b(VACUUM\s+FULL|CLUSTER\s+[a-zA-Z_][\w$]*|REINDEX\s+(TABLE|INDEX|DATABASE|SYSTEM)\b|ALTER\s+TABLE[\s\S]*ALTER\s+COLUMN[\s\S]*TYPE\b)`)
	lockTimeoutPattern      = regexp.MustCompile(`(?is)\b(SET(\s+LOCAL)?\s+lock_timeout\b|lock_timeout\s*=)`)
	statementTimeoutPattern = regexp.MustCompile(`(?is)\b(SET(\s+LOCAL)?\s+statement_timeout\b|statement_timeout\s*=)`)
	ddlVerbPattern          = regexp.MustCompile(`(?is)^\s*(CREATE|ALTER|DROP|REINDEX|CLUSTER|VACUUM|TRUNCATE)\b`)
	integerLiteralPattern   = regexp.MustCompile(`^[+-]?\d+$`)
	sqlStringTokenPattern   = regexp.MustCompile(`^__SQL_STR_[0-9a-f]+__$`)
	trailingClausePattern   = regexp.MustCompile(`(?is)\b(LIMIT|OFFSET|FETCH|ORDER\s+BY|GROUP\s+BY|HAVING|RETURNING)\b`)
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
		pattern, err := compilePolicyPattern(policy.Pattern)
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
	combinedSQL := stripSQLCommentsAndStrings(sql) + "\n" + stripSQLCommentsAndStrings(rollbackSQL)

	for _, statement := range statements {
		codeStmt := strings.TrimSpace(stripSQLCommentsAndStrings(statement))
		if codeStmt != "" {
			upper := strings.ToUpper(codeStmt)
			dmlKind, dmlMain := findTopLevelDML(codeStmt)
			isUpdate := dmlKind == "UPDATE"
			isDelete := dmlKind == "DELETE"
			whereExpr, hasWhere := topLevelWhereClause(dmlMain)
			tautological := hasWhere && hasTautologicalWhere(whereExpr)
			effectiveWhere := hasWhere && !tautological
			hasBatchBound := limitPattern.MatchString(codeStmt) || batchHintPattern.MatchString(codeStmt)

			if isUpdate && !effectiveWhere {
				add("UPDATE_WITHOUT_WHERE", "语句可能更新整张表。", compact(statement))
			}
			if isDelete && !effectiveWhere {
				add("DELETE_WITHOUT_WHERE", "语句可能删除整张表的数据。", compact(statement))
			}
			// Bounded DML still needs batching when it can touch many rows in one TX.
			if (isUpdate || isDelete) && effectiveWhere && !hasBatchBound {
				add("UNBATCHED_LARGE_DML", "条件 DML 缺少 LIMIT/分批边界，可能在单事务中锁住大量行并拉长 WAL/回滚窗口。", compact(statement))
				hasBulkDML = true
				needsLockTimeout = true
			}
			if (isUpdate || isDelete) && hasBatchBound {
				hasBulkDML = true
				needsLockTimeout = true
			}
			if strings.Contains(upper, " ADD ") && strings.Contains(upper, " NOT NULL") && !defaultKeywordPattern.MatchString(codeStmt) {
				add("ADD_NOT_NULL_WITHOUT_DEFAULT", "历史数据无法自动满足非空约束，迁移可能直接失败。", compact(statement))
				needsLockTimeout = true
			}
			if createIndexPattern.MatchString(codeStmt) && !concurrentIndexPattern.MatchString(codeStmt) {
				add("INDEX_NOT_CONCURRENT", "高写入表直接创建索引可能扩大锁等待窗口。", compact(statement))
				needsLockTimeout = true
				hasDDL = true
			}
			// Foreign keys without NOT VALID force a full table scan under lock.
			if (addConstraintPattern.MatchString(codeStmt) || (strings.Contains(upper, "ADD CONSTRAINT") && foreignKeyPattern.MatchString(codeStmt)) || (strings.Contains(upper, "FOREIGN KEY") && strings.Contains(upper, "REFERENCES"))) && !notValidPattern.MatchString(codeStmt) {
				add("FK_WITHOUT_NOT_VALID", "直接添加外键会在创建时整表校验并持有较长时间锁。", compact(statement))
				needsLockTimeout = true
				hasDDL = true
			}
			if forUpdatePattern.MatchString(codeStmt) && !limitPattern.MatchString(codeStmt) && !hasPrimaryKeyPredicate(upper) {
				add("SELECT_FOR_UPDATE_UNBOUNDED", "FOR UPDATE 缺少 LIMIT 或明确主键范围，可能锁住过量行。", compact(statement))
				needsLockTimeout = true
			}
			if heavyDDLPattern.MatchString(codeStmt) {
				add("HEAVY_DDL_REWRITE", "重写型 DDL 可能触发表重写与长事务，阻塞读写。", compact(statement))
				needsLockTimeout = true
				hasDDL = true
			}
			if ddlVerbPattern.MatchString(codeStmt) {
				hasDDL = true
				if strings.Contains(upper, "ALTER TABLE") || strings.Contains(upper, "DROP INDEX") || strings.Contains(upper, "CREATE INDEX") {
					needsLockTimeout = true
				}
			}
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
	var blockDepth int
	var dollarTag string
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
			if ch == '/' && next == '*' {
				buf.WriteByte(next)
				i++
				blockDepth++
				continue
			}
			if ch == '*' && next == '/' {
				buf.WriteByte(next)
				i++
				blockDepth--
				if blockDepth <= 0 {
					blockComment = false
					blockDepth = 0
				}
			}
			continue
		}
		if dollarTag != "" {
			if strings.HasPrefix(input[i:], dollarTag) {
				buf.WriteString(dollarTag)
				i += len(dollarTag) - 1
				dollarTag = ""
				continue
			}
			buf.WriteByte(ch)
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
			blockDepth = 1
			buf.WriteByte(ch)
			buf.WriteByte(next)
			i++
			continue
		}
		if !single && !double && ch == '$' {
			if tag, ok := matchDollarTag(input, i); ok {
				dollarTag = tag
				buf.WriteString(tag)
				i += len(tag) - 1
				continue
			}
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

func isSQLIdentByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '$'
}

func matchDollarTag(input string, pos int) (string, bool) {
	if pos < 0 || pos >= len(input) || input[pos] != '$' {
		return "", false
	}
	if pos > 0 && isSQLIdentByte(input[pos-1]) {
		return "", false
	}
	end := -1
	for j := pos + 1; j < len(input); j++ {
		c := input[j]
		if c == '$' {
			end = j
			break
		}
		if j == pos+1 {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return "", false
			}
		} else if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
			return "", false
		}
	}
	if end < 0 {
		return "", false
	}
	tag := input[pos : end+1]
	if !strings.Contains(input[end+1:], tag) {
		return "", false
	}
	return tag, true
}

func stringLiteralToken(raw string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	return fmt.Sprintf(" __SQL_STR_%08x__ ", h.Sum32())
}

// stripSQLCommentsAndStrings removes SQL comments (-- and /* */) and replaces
// single-quoted / dollar-quoted string literals with deterministic literal
// tokens so comments and strings cannot spoof or hide SQL keywords while
// double-quoted identifiers remain intact.
func stripSQLCommentsAndStrings(input string) string {
	var buf strings.Builder
	buf.Grow(len(input))
	for i := 0; i < len(input); i++ {
		ch := input[i]
		var next byte
		if i+1 < len(input) {
			next = input[i+1]
		}
		if ch == '-' && next == '-' {
			buf.WriteByte(' ')
			i += 2
			for i < len(input) && input[i] != '\n' {
				i++
			}
			if i < len(input) && input[i] == '\n' {
				buf.WriteByte('\n')
			}
			continue
		}
		if ch == '/' && next == '*' {
			depth := 1
			i += 2
			for i < len(input) && depth > 0 {
				if input[i] == '/' && i+1 < len(input) && input[i+1] == '*' {
					depth++
					i += 2
					continue
				}
				if input[i] == '*' && i+1 < len(input) && input[i+1] == '/' {
					depth--
					i += 2
					continue
				}
				i++
			}
			i--
			buf.WriteByte(' ')
			continue
		}
		if ch == '\'' {
			escaped := i > 0 && (input[i-1] == 'E' || input[i-1] == 'e') && (i < 2 || !isSQLIdentByte(input[i-2]))
			start := i + 1
			i++
			for i < len(input) {
				if escaped && input[i] == '\\' && i+1 < len(input) {
					i += 2
					continue
				}
				if input[i] == '\'' {
					if i+1 < len(input) && input[i+1] == '\'' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			end := i
			if end > len(input) {
				end = len(input)
			}
			buf.WriteString(stringLiteralToken(input[start:end]))
			continue
		}
		if ch == '"' {
			buf.WriteByte(ch)
			i++
			for i < len(input) {
				buf.WriteByte(input[i])
				if input[i] == '"' {
					if i+1 < len(input) && input[i+1] == '"' {
						i++
						buf.WriteByte(input[i])
						i++
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if ch == '$' {
			if tag, ok := matchDollarTag(input, i); ok {
				start := i + len(tag)
				relEnd := strings.Index(input[start:], tag)
				buf.WriteString(stringLiteralToken(input[start : start+relEnd]))
				i = start + relEnd + len(tag) - 1
				continue
			}
		}
		buf.WriteByte(ch)
	}
	return buf.String()
}

func skipSpace(s string, pos int) int {
	for pos < len(s) && (s[pos] == ' ' || s[pos] == '\t' || s[pos] == '\n' || s[pos] == '\r') {
		pos++
	}
	return pos
}

func readWord(s string, pos int) (string, int) {
	pos = skipSpace(s, pos)
	start := pos
	for pos < len(s) && isSQLIdentByte(s[pos]) {
		pos++
	}
	return s[start:pos], pos
}

func skipParenGroup(s string, pos int) (int, int, bool) {
	pos = skipSpace(s, pos)
	if pos >= len(s) || s[pos] != '(' {
		return pos, pos, false
	}
	start := pos + 1
	depth := 1
	for i := start; i < len(s); i++ {
		if s[i] == '"' {
			i++
			for i < len(s) {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if s[i] == '(' {
			depth++
		} else if s[i] == ')' {
			depth--
			if depth == 0 {
				return start, i, true
			}
		}
	}
	return start, len(s), false
}

func findTopLevelDML(codeStmt string) (string, string) {
	trimmed := strings.TrimSpace(codeStmt)
	word, pos := readWord(trimmed, 0)
	upper := strings.ToUpper(word)
	if upper == "UPDATE" || upper == "DELETE" {
		return upper, trimmed
	}
	if upper != "WITH" {
		return "", ""
	}
	var cteDMLKind, cteDMLClause string
	nextWord, afterNext := readWord(trimmed, pos)
	if strings.EqualFold(nextWord, "RECURSIVE") {
		pos = afterNext
	}
	for pos < len(trimmed) {
		_, afterName := readWord(trimmed, pos)
		if afterName == pos {
			break
		}
		pos = skipSpace(trimmed, afterName)
		if pos < len(trimmed) && trimmed[pos] == '(' {
			_, endIdx, ok := skipParenGroup(trimmed, pos)
			if !ok {
				return "", ""
			}
			pos = endIdx + 1
		}
		kw, afterKW := readWord(trimmed, pos)
		if !strings.EqualFold(kw, "AS") {
			break
		}
		pos = skipSpace(trimmed, afterKW)
		// Optional PostgreSQL MATERIALIZED / NOT MATERIALIZED hint
		opt1, afterOpt1 := readWord(trimmed, pos)
		if strings.EqualFold(opt1, "NOT") {
			opt2, afterOpt2 := readWord(trimmed, afterOpt1)
			if strings.EqualFold(opt2, "MATERIALIZED") {
				pos = afterOpt2
			}
		} else if strings.EqualFold(opt1, "MATERIALIZED") {
			pos = afterOpt1
		}
		bodyStart, bodyEnd, ok := skipParenGroup(trimmed, pos)
		if !ok {
			return "", ""
		}
		if cteDMLKind == "" {
			body := strings.TrimSpace(trimmed[bodyStart:bodyEnd])
			bodyVerb, _ := readWord(body, 0)
			upperBodyVerb := strings.ToUpper(bodyVerb)
			if upperBodyVerb == "UPDATE" || upperBodyVerb == "DELETE" {
				cteDMLKind = upperBodyVerb
				cteDMLClause = body
			}
		}
		pos = skipSpace(trimmed, bodyEnd+1)
		if pos < len(trimmed) && trimmed[pos] == ',' {
			pos++
			continue
		}
		break
	}
	pos = skipSpace(trimmed, pos)
	if pos < len(trimmed) {
		mainClause := strings.TrimSpace(trimmed[pos:])
		mainVerb, _ := readWord(mainClause, 0)
		upperMainVerb := strings.ToUpper(mainVerb)
		if upperMainVerb == "UPDATE" || upperMainVerb == "DELETE" {
			return upperMainVerb, mainClause
		}
	}
	return cteDMLKind, cteDMLClause
}

func topLevelWhereClause(clause string) (string, bool) {
	if strings.TrimSpace(clause) == "" {
		return "", false
	}
	depth := 0
	wherePos := -1
	for i := 0; i < len(clause); i++ {
		if clause[i] == '"' {
			i++
			for i < len(clause) {
				if clause[i] == '"' {
					if i+1 < len(clause) && clause[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if clause[i] == '(' {
			depth++
			continue
		}
		if clause[i] == ')' {
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if isSQLIdentByte(clause[i]) {
			start := i
			for i < len(clause) && isSQLIdentByte(clause[i]) {
				i++
			}
			if strings.EqualFold(clause[start:i], "WHERE") {
				wherePos = i
				break
			}
			i--
		}
	}
	if wherePos < 0 {
		return "", false
	}
	rest := clause[wherePos:]
	depth = 0
	end := len(rest)
	for i := 0; i < len(rest); i++ {
		if rest[i] == '"' {
			i++
			for i < len(rest) {
				if rest[i] == '"' {
					if i+1 < len(rest) && rest[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if rest[i] == '(' {
			depth++
			continue
		}
		if rest[i] == ')' {
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if rest[i] == ';' {
			end = i
			break
		}
		if isSQLIdentByte(rest[i]) {
			start := i
			for i < len(rest) && isSQLIdentByte(rest[i]) {
				i++
			}
			kw := strings.ToUpper(rest[start:i])
			if kw == "LIMIT" || kw == "OFFSET" || kw == "FETCH" || kw == "HAVING" || kw == "RETURNING" {
				end = start
				break
			}
			if kw == "ORDER" || kw == "GROUP" {
				next, _ := readWord(rest, i)
				if strings.EqualFold(next, "BY") {
					end = start
					break
				}
			}
			i--
		}
	}
	return strings.TrimSpace(rest[:end]), true
}

func stripOuterParens(expr string) string {
	expr = strings.TrimSpace(expr)
	for len(expr) >= 2 && expr[0] == '(' && expr[len(expr)-1] == ')' {
		depth := 0
		matched := true
		for i := 0; i < len(expr); i++ {
			if expr[i] == '"' {
				i++
				for i < len(expr) {
					if expr[i] == '"' {
						if i+1 < len(expr) && expr[i+1] == '"' {
							i += 2
							continue
						}
						break
					}
					i++
				}
				continue
			}
			if expr[i] == '(' {
				depth++
			} else if expr[i] == ')' {
				depth--
				if depth == 0 && i < len(expr)-1 {
					matched = false
					break
				}
			}
		}
		if !matched || depth != 0 {
			break
		}
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	return expr
}

func splitTopLevelKeyword(expr, keyword string) []string {
	var parts []string
	depth := 0
	last := 0
	for i := 0; i < len(expr); i++ {
		if expr[i] == '"' {
			i++
			for i < len(expr) {
				if expr[i] == '"' {
					if i+1 < len(expr) && expr[i+1] == '"' {
						i += 2
						continue
					}
					break
				}
				i++
			}
			continue
		}
		if expr[i] == '(' {
			depth++
			continue
		}
		if expr[i] == ')' {
			if depth > 0 {
				depth--
			}
			continue
		}
		if depth != 0 {
			continue
		}
		if isSQLIdentByte(expr[i]) {
			start := i
			for i < len(expr) && isSQLIdentByte(expr[i]) {
				i++
			}
			if strings.EqualFold(expr[start:i], keyword) {
				parts = append(parts, strings.TrimSpace(expr[last:start]))
				last = i
			}
			i--
		}
	}
	parts = append(parts, strings.TrimSpace(expr[last:]))
	return parts
}

func hasTautologicalWhere(whereExpr string) bool {
	expr := stripOuterParens(strings.Join(strings.Fields(whereExpr), " "))
	if expr == "" {
		return false
	}
	for _, branch := range splitTopLevelKeyword(expr, "OR") {
		if isAndBranchTautological(branch) {
			return true
		}
	}
	return false
}

func isAndBranchTautological(branch string) bool {
	branch = stripOuterParens(strings.TrimSpace(branch))
	if branch == "" {
		return false
	}
	terms := splitTopLevelKeyword(branch, "AND")
	if len(terms) == 0 {
		return false
	}
	for _, term := range terms {
		if !isTautologyTerm(term) {
			return false
		}
	}
	return true
}

func isTautologyTerm(term string) bool {
	raw := strings.TrimSpace(term)
	unwrapped := stripOuterParens(raw)
	if unwrapped != raw {
		return hasTautologicalWhere(unwrapped)
	}
	if unwrapped == "" {
		return false
	}
	if len(splitTopLevelKeyword(unwrapped, "OR")) > 1 {
		return hasTautologicalWhere(unwrapped)
	}
	if len(splitTopLevelKeyword(unwrapped, "AND")) > 1 {
		return isAndBranchTautological(unwrapped)
	}
	upper := strings.ToUpper(strings.Join(strings.Fields(unwrapped), " "))
	if upper == "TRUE" {
		return true
	}
	if strings.HasPrefix(upper, "NOT ") {
		inner := stripOuterParens(strings.TrimSpace(unwrapped[4:]))
		return isContradictionTerm(inner)
	}
	return evalLiteralComparison(unwrapped, true)
}

func isContradictionTerm(term string) bool {
	unwrapped := stripOuterParens(strings.TrimSpace(term))
	if strings.EqualFold(unwrapped, "FALSE") {
		return true
	}
	return evalLiteralComparison(unwrapped, false)
}

func evalLiteralComparison(expr string, wantTrue bool) bool {
	for _, op := range []string{"<>", "!=", "="} {
		idx := strings.Index(expr, op)
		if idx < 0 {
			continue
		}
		left := strings.TrimSpace(expr[:idx])
		right := strings.TrimSpace(expr[idx+len(op):])
		if left == "" || right == "" {
			continue
		}
		var equal bool
		if integerLiteralPattern.MatchString(left) && integerLiteralPattern.MatchString(right) {
			lv, lerr := strconv.ParseInt(left, 10, 64)
			rv, rerr := strconv.ParseInt(right, 10, 64)
			if lerr != nil || rerr != nil {
				return false
			}
			equal = lv == rv
		} else if sqlStringTokenPattern.MatchString(left) && sqlStringTokenPattern.MatchString(right) {
			equal = left == right
		} else if (strings.EqualFold(left, "TRUE") || strings.EqualFold(left, "FALSE")) && (strings.EqualFold(right, "TRUE") || strings.EqualFold(right, "FALSE")) {
			equal = strings.EqualFold(left, right)
		} else {
			return false
		}
		if op == "=" {
			return equal == wantTrue
		}
		return (!equal) == wantTrue
	}
	return false
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
