"""确定性 SQL 扫描。

**这是检查结果的唯一来源。模型只能读它，不能覆盖、不能清空。**

规则与 `examples/agent-demo/norms/sql-change-standards.md` 一一对应，
每条规则的 suggestion 都会指回具体条款，便于人工核对。
"""

from __future__ import annotations

import re
from datetime import datetime, timezone

from app.schemas.drafts import CheckItem, DeterministicCheck

_DDL = re.compile(r"(?is)\b(create|alter|drop|truncate|reindex|cluster|vacuum)\b")
_CREATE_INDEX = re.compile(r"(?is)\bcreate\s+(unique\s+)?index\s+(concurrently\s+)?", re.IGNORECASE)
_DROP_INDEX = re.compile(r"(?is)\bdrop\s+index\s+(concurrently\s+)?", re.IGNORECASE)
_TRANSACTION = re.compile(r"(?is)^\s*(begin|start\s+transaction)\b", re.MULTILINE)
_LOCK_TIMEOUT = re.compile(r"(?is)\block_timeout\b")
_UPDATE_WITHOUT_WHERE = re.compile(r"(?is)\bupdate\s+[A-Za-z_][\w.]*\s+set\b(?![^;]*\bwhere\b)")
_DELETE_WITHOUT_WHERE = re.compile(r"(?is)\bdelete\s+from\s+[A-Za-z_][\w.]*\s*(?:;|$)(?![^;]*\bwhere\b)")
_DROP_TABLE = re.compile(r"(?is)\bdrop\s+table\b")
_TRUNCATE = re.compile(r"(?is)\btruncate\b")

HOT_TABLE_MARKERS = ("热表", "hot table", "高写入", "日均写入")


def scan_sql(sql: str, rollback_sql: str = "", schema_snapshot: str = "") -> DeterministicCheck:
    """对草案 SQL 做确定性扫描。

    不调用模型、不访问网络，相同输入必然得到相同结果。
    """
    items: list[CheckItem] = []
    text = sql or ""
    rollback = rollback_sql or ""

    if not text.strip():
        items.append(
            CheckItem(
                code="SQL_EMPTY",
                severity="HIGH",
                blocking=True,
                title="变更 SQL 为空",
                suggestion="至少提供一条可执行的变更语句（规范 4.1）。",
            )
        )

    if not rollback.strip():
        items.append(
            CheckItem(
                code="MISSING_ROLLBACK",
                severity="HIGH",
                blocking=True,
                title="缺少回滚语句",
                suggestion="提供可执行的回滚语句；确实不可回滚时必须在材料中说明理由（规范 4.1）。",
            )
        )

    # 规范 1.1 / 1.2：热表必须并发建索引，且并发语句不能放在事务里。
    for match in _CREATE_INDEX.finditer(text):
        is_concurrent = bool(match.group(2))
        if is_concurrent:
            continue
        if _is_hot_table(schema_snapshot):
            items.append(
                CheckItem(
                    code="INDEX_NOT_CONCURRENT",
                    severity="HIGH",
                    blocking=True,
                    title="热表使用了非并发建索引",
                    suggestion="改为 CREATE INDEX CONCURRENTLY；非并发方式会持有 ACCESS EXCLUSIVE 锁（规范 1.1，参见 CASE-002）。",
                )
            )
        else:
            items.append(
                CheckItem(
                    code="INDEX_CONCURRENCY_UNKNOWN",
                    severity="MEDIUM",
                    blocking=False,
                    title="无法确认表是否为热表，索引未使用并发方式",
                    suggestion="补充表写入量证据；若为热表必须改为 CREATE INDEX CONCURRENTLY（规范 1.1）。",
                )
            )

    if _TRANSACTION.search(text) and re.search(r"(?is)\bconcurrently\b", text):
        items.append(
            CheckItem(
                code="CONCURRENTLY_IN_TRANSACTION",
                severity="HIGH",
                blocking=True,
                title="并发语句出现在事务块中",
                suggestion="移除 BEGIN/COMMIT；CREATE INDEX CONCURRENTLY 无法在事务内执行（规范 1.2）。",
            )
        )

    if _DDL.search(text) and not _LOCK_TIMEOUT.search(text):
        items.append(
            CheckItem(
                code="MISSING_LOCK_TIMEOUT",
                severity="MEDIUM",
                blocking=False,
                title="DDL 未设置 lock_timeout",
                suggestion="在执行 DDL 前设置 lock_timeout（建议 3s），避免长时间排队持锁（规范 2.1）。",
            )
        )

    for match in _DROP_INDEX.finditer(rollback or text):
        if match.group(1):
            continue
        items.append(
            CheckItem(
                code="DROP_INDEX_NOT_CONCURRENT",
                severity="LOW",
                blocking=False,
                title="删除索引未使用并发方式",
                suggestion="改为 DROP INDEX CONCURRENTLY，避免阻塞读写（回滚手册 1.1）。",
            )
        )

    if _UPDATE_WITHOUT_WHERE.search(text):
        items.append(
            CheckItem(
                code="UPDATE_WITHOUT_WHERE",
                severity="HIGH",
                blocking=True,
                title="无条件 UPDATE",
                suggestion="UPDATE 必须带 WHERE 并说明影响行数估算依据（规范 3.1）。",
            )
        )

    if _DELETE_WITHOUT_WHERE.search(text):
        items.append(
            CheckItem(
                code="DELETE_WITHOUT_WHERE",
                severity="HIGH",
                blocking=True,
                title="无条件 DELETE",
                suggestion="DELETE 必须带 WHERE 并说明影响行数估算依据（规范 3.1）。",
            )
        )

    if _DROP_TABLE.search(text):
        items.append(
            CheckItem(
                code="DROP_TABLE",
                severity="HIGH",
                blocking=True,
                title="包含 DROP TABLE",
                suggestion="删除表属于不可逆操作，需单独评审并说明数据处置方案。",
            )
        )

    if _TRUNCATE.search(text):
        items.append(
            CheckItem(
                code="TRUNCATE",
                severity="HIGH",
                blocking=True,
                title="包含 TRUNCATE",
                suggestion="TRUNCATE 会立即清空数据且难以回滚，需单独评审。",
            )
        )

    blocking_count = sum(1 for item in items if item.blocking)
    if not text.strip():
        status = "FAILED"
    elif blocking_count:
        status = "BLOCKED"
    else:
        status = "PASSED"

    return DeterministicCheck(
        status=status,
        source="local_scan",
        checked_at=datetime.now(timezone.utc),
        items=items,
        blocking_count=blocking_count,
    )


def _is_hot_table(schema_snapshot: str) -> bool:
    lowered = (schema_snapshot or "").lower()
    return any(marker.lower() in lowered for marker in HOT_TABLE_MARKERS)


def feedback_lines(check: DeterministicCheck) -> list[str]:
    """把检查结果转成给模型看的反馈行（不含任何"通过"暗示）。"""
    return [f"{item.code}: {item.title} —— {item.suggestion}" for item in check.items]
