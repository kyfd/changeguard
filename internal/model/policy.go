package model

import "time"

type RiskPolicy struct {
	OrganizationID string     `json:"organization_id"`
	ID             string     `json:"id"`
	Code           string     `json:"code"`
	Name           string     `json:"name"`
	Description    string     `json:"description"`
	Pattern        string     `json:"pattern,omitempty"`
	Severity       RiskLevel  `json:"severity"`
	Blocking       bool       `json:"blocking"`
	Enabled        bool       `json:"enabled"`
	Builtin        bool       `json:"builtin"`
	Environments   []string   `json:"environments,omitempty"`
	ChangeTypes    []string   `json:"change_types,omitempty"`
	ArtifactKinds  []string   `json:"artifact_kinds,omitempty"`
	Suggestion     string     `json:"suggestion"`
	Version        int        `json:"version"`
	HitCount       int64      `json:"hit_count"`
	LastHitAt      *time.Time `json:"last_hit_at,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	UpdatedBy      string     `json:"updated_by"`
}

type SaveRiskPolicyInput struct {
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Description   string    `json:"description"`
	Pattern       string    `json:"pattern"`
	Severity      RiskLevel `json:"severity"`
	Blocking      bool      `json:"blocking"`
	Enabled       bool      `json:"enabled"`
	Environments  []string  `json:"environments"`
	ChangeTypes   []string  `json:"change_types"`
	ArtifactKinds []string  `json:"artifact_kinds"`
	Suggestion    string    `json:"suggestion"`
}

type TestRiskPolicyInput struct {
	SQL          string `json:"sql"`
	RollbackSQL  string `json:"rollback_sql"`
	Environment  string `json:"environment"`
	ChangeType   string `json:"change_type"`
	ArtifactKind string `json:"artifact_kind"`
	Content      string `json:"content"`
}

func DefaultRiskPolicies(now time.Time) []RiskPolicy {
	definitions := []RiskPolicy{
		{ID: "pol_empty_sql", Code: "EMPTY_SQL", Name: "变更 SQL 不能为空", Description: "阻止没有执行语句的空变更单进入后续流程。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "补充需要执行的 SQL 后重新提交。"},
		{ID: "pol_missing_rollback", Code: "MISSING_ROLLBACK", Name: "必须提供回滚方案", Description: "生产数据库变更必须登记可以验证的回滚 SQL。", Severity: RiskMedium, Blocking: true, Enabled: true, Builtin: true, Suggestion: "补充回滚 SQL，无法回滚时需走独立高风险审批。"},
		{ID: "pol_many_statements", Code: "TOO_MANY_STATEMENTS", Name: "限制单次 SQL 语句数量", Description: "单个变更包含过多语句时，失败定位和回滚成本显著增加。", Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "按业务目标和回滚边界拆分变更单。"},
		{ID: "pol_update_all", Code: "UPDATE_WITHOUT_WHERE", Name: "禁止无条件全表更新", Description: "UPDATE 缺少 WHERE 时可能修改整张表。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "补充明确条件，并先用 SELECT 核对影响范围。"},
		{ID: "pol_delete_all", Code: "DELETE_WITHOUT_WHERE", Name: "禁止无条件全表删除", Description: "DELETE 缺少 WHERE 时可能删除整张表数据。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "补充明确条件并确认备份和影响行数。"},
		{ID: "pol_not_null_default", Code: "ADD_NOT_NULL_WITHOUT_DEFAULT", Name: "新增非空字段必须提供迁移方案", Description: "历史数据无法直接满足新增非空约束。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "先增加可空字段，分批回填后再添加非空约束。"},
		{ID: "pol_index_concurrent", Code: "INDEX_NOT_CONCURRENT", Name: "高写入表索引并发创建", Description: "直接创建索引可能扩大锁等待窗口。", Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "评估使用 CONCURRENTLY，并在低峰期观察锁等待。"},
		{ID: "pol_transaction_control", Code: "TRANSACTION_CONTROL", Name: "禁止事务控制语句", Description: "用户 SQL 不能结束或切换影子演练事务，否则可能绕过自动回滚。", Pattern: `(?im)^\s*(BEGIN|START\s+TRANSACTION|COMMIT|END|ROLLBACK|SAVEPOINT|RELEASE\s+SAVEPOINT|PREPARE\s+TRANSACTION|COMMIT\s+PREPARED|ROLLBACK\s+PREPARED)\b`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "移除事务控制语句，由影子演练执行器统一管理事务。"},
		{ID: "pol_psql_meta", Code: "PSQL_META_COMMAND", Name: "禁止 psql 元命令", Description: "反斜杠元命令可能读写文件或执行外部程序，不能进入演练环境。", Pattern: `(?m)^\s*\\`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: `只提交标准 PostgreSQL SQL，不要包含 \copy、\!、\gexec 等 psql 命令。`},
		{ID: "pol_drop_table", Code: "DROP_TABLE", Name: "删除表高风险检查", Description: "删除表属于不可逆或高恢复成本操作。", Pattern: `(?is)\bDROP\s+TABLE\b`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "优先归档或重命名，验证备份后单独审批。"},
		{ID: "pol_drop_column", Code: "DROP_COLUMN", Name: "删除字段依赖检查", Description: "旧版本服务或离线任务可能仍在读取该字段。", Pattern: `(?is)\bDROP\s+COLUMN\b`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "先完成依赖扫描和读路径下线，再分阶段删除。"},
		{ID: "pol_alter_type", Code: "ALTER_COLUMN_TYPE", Name: "字段类型变更检查", Description: "字段类型转换可能引发表重写、长事务和磁盘放大。", Pattern: `(?is)ALTER\s+TABLE[\s\S]*ALTER\s+COLUMN[\s\S]*TYPE\b`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "在影子库按接近生产的数据量验证耗时。"},
		{ID: "pol_unique_index", Code: "UNIQUE_INDEX", Name: "唯一索引历史重复检查", Description: "存量重复值会导致唯一索引创建失败。", Pattern: `(?is)CREATE\s+UNIQUE\s+INDEX\b`, Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "先统计重复值并制定清理方案。"},
		{ID: "pol_set_not_null", Code: "SET_NOT_NULL", Name: "非空约束历史数据检查", Description: "添加非空约束前需要确认历史行全部满足条件。", Pattern: `(?is)ALTER\s+(COLUMN\s+)?[a-zA-Z_][\w$]*\s+SET\s+NOT\s+NULL`, Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "先检查空值数量，必要时分阶段添加约束。"},
		{ID: "pol_truncate", Code: "TRUNCATE", Name: "清空表高风险检查", Description: "TRUNCATE 影响范围大且通常不能按行恢复。", Pattern: `(?is)\bTRUNCATE\b`, Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "单独审批并验证备份与恢复演练。"},
		{ID: "pol_release_rollback", Code: "MISSING_RELEASE_ROLLBACK", Name: "发布变更必须提供回滚方案", Description: "代码、配置、Kubernetes 和 API 变更必须说明可执行的恢复方式。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "填写版本回退、配置恢复、流量切换或数据补偿步骤。"},
		{ID: "pol_k8s_latest", Code: "K8S_LATEST_IMAGE", Name: "禁止生产镜像使用 latest 标签", Description: "latest 无法唯一定位版本，也会破坏回滚和审计。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "使用不可变版本号或镜像 digest。"},
		{ID: "pol_k8s_privileged", Code: "K8S_PRIVILEGED", Name: "禁止容器开启特权模式", Description: "privileged 容器拥有接近宿主机的权限。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "关闭 privileged，并按最小权限补充 capabilities。"},
		{ID: "pol_k8s_resources", Code: "K8S_RESOURCE_LIMITS", Name: "工作负载必须配置资源约束", Description: "缺少 requests 或 limits 会影响调度稳定性和故障隔离。", Severity: RiskMedium, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "根据压测结果配置 CPU、内存 requests 与 limits。"},
		{ID: "pol_k8s_host_namespace", Code: "K8S_HOST_NAMESPACE", Name: "禁止共享宿主机命名空间", Description: "hostNetwork 或 hostPID 会扩大横向移动和宿主机信息暴露风险。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "关闭 hostNetwork/hostPID，使用集群网络与标准服务发现。"},
		{ID: "pol_k8s_escalation", Code: "K8S_PRIVILEGE_ESCALATION", Name: "禁止容器提升权限", Description: "allowPrivilegeEscalation 会允许进程获得超出镜像基线的权限。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "设置 allowPrivilegeEscalation: false 并收敛 capabilities。"},
		{ID: "pol_k8s_root", Code: "K8S_RUN_AS_ROOT", Name: "禁止容器以 root 身份运行", Description: "root 容器在漏洞利用后具有更大的节点影响面。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "设置 runAsNonRoot: true 和非零 runAsUser。"},
		{ID: "pol_k8s_probes", Code: "K8S_HEALTH_PROBES", Name: "长期运行容器必须配置健康探针", Description: "缺少就绪和存活探针会导致故障实例继续接流量或无法自愈。", Severity: RiskMedium, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "配置 readinessProbe 与 livenessProbe，并使用独立健康端点。"},
		{ID: "pol_k8s_replica", Code: "K8S_SINGLE_REPLICA", Name: "生产服务不得单副本运行", Description: "单副本在滚动升级和节点故障时会直接中断。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "生产 Deployment/StatefulSet 至少配置两个副本并设置 PDB。"},
		{ID: "pol_k8s_host_path", Code: "K8S_HOST_PATH", Name: "限制生产工作负载挂载 hostPath", Description: "hostPath 绕过容器文件系统隔离并绑定具体节点。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactKubernetes)}, Suggestion: "改用 PVC、CSI 或受控临时卷；确需使用时走独立安全审批。"},
		{ID: "pol_config_secret", Code: "CONFIG_SECRET_EXPOSURE", Name: "配置中疑似包含明文密钥", Description: "口令、Token 或 Secret 不应直接进入代码仓库和变更证据。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactConfig)}, Suggestion: "改用企业密钥管理服务或 Kubernetes Secret 引用。"},
		{ID: "pol_config_debug", Code: "CONFIG_DEBUG_ENABLED", Name: "生产环境禁止开启调试模式", Description: "调试模式可能暴露内部信息并增加性能开销。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactConfig)}, Suggestion: "生产配置关闭 debug、trace 和详细错误输出。"},
		{ID: "pol_config_auth", Code: "CONFIG_AUTH_DISABLED", Name: "生产环境不得关闭认证授权", Description: "关闭认证、授权或开放匿名访问会使内部能力直接暴露。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactConfig)}, Suggestion: "恢复认证授权基线，并通过最小权限角色开放访问。"},
		{ID: "pol_config_tls", Code: "CONFIG_TLS_VERIFY_DISABLED", Name: "生产环境不得跳过 TLS 校验", Description: "跳过证书校验会使服务间通信暴露于中间人攻击。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, ArtifactKinds: []string{string(ArtifactConfig)}, Suggestion: "启用证书链和主机名校验，修复信任链而不是关闭验证。"},
		{ID: "pol_full_release", Code: "PRODUCTION_FULL_RELEASE", Name: "生产发布缺少灰度策略", Description: "核心服务直接全量发布会扩大故障影响半径。", Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "使用金丝雀或蓝绿发布，并设置观察窗口。"},
		{ID: "pol_missing_metrics", Code: "MISSING_OBSERVATION_METRICS", Name: "发布计划缺少成功判定指标", Description: "没有错误率、延迟或业务指标时无法自动判断发布是否安全。", Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "至少配置错误率、P99 和一个核心业务指标。"},
		{ID: "pol_change_window_conflict", Code: "CHANGE_WINDOW_CONFLICT", Name: "同一服务发布窗口冲突", Description: "同一服务在保护窗口内安排多个未闭环变更，容易造成版本覆盖、回滚基线混乱和责任边界不清。", Severity: RiskHigh, Blocking: true, Enabled: true, Builtin: true, Suggestion: "调整计划时间，或先关闭已有变更并确认唯一发布负责人、版本与回滚基线。"},
		{ID: "pol_dependency_window_overlap", Code: "DEPENDENCY_WINDOW_OVERLAP", Name: "上下游服务发布窗口重叠", Description: "直接依赖或被依赖服务同时发布，会增加联调失败和故障归因成本。", Severity: RiskMedium, Enabled: true, Builtin: true, Suggestion: "与上下游负责人确认兼容顺序、联动观察指标和联合回滚方案，必要时错峰发布。"},
	}
	for index := range definitions {
		definitions[index].Version = 1
		definitions[index].CreatedAt = now
		definitions[index].UpdatedAt = now
		definitions[index].UpdatedBy = "系统内置"
	}
	return definitions
}
