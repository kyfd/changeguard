"""模型接入层。

两个实现共用同一个契约：**返回一段文本，由上层严格解析成 Draft**。

- `OpenAICompatibleProvider`：真实模型，带超时、重试与并发限制。
- `DeterministicProvider`：不依赖模型的兜底实现。

关键设计：离线兜底也走**同一条严格解析路径**。这样"模型返回非法 JSON"的
处理逻辑在离线演示中同样被执行，不会出现"只有线上才跑那段代码"。
"""

from __future__ import annotations

import asyncio
import json
import re
from typing import Any, Protocol

import httpx
from pydantic import BaseModel, Field

from app.config import Settings
from app.schemas.drafts import Assumption, DatabaseKind, EvidenceRef, TaskSlots


class DraftRequest(BaseModel):
    """生成草案的输入。"""

    requirement: str
    slots: TaskSlots
    schema_snapshot: str = ""
    evidence: list[EvidenceRef] = Field(default_factory=list)
    previous_draft: str | None = None
    check_feedback: list[str] = Field(default_factory=list)
    revision: int = 0


class DraftProvider(Protocol):
    """草案生成器契约。"""

    name: str

    async def generate(self, request: DraftRequest) -> str:
        """返回原始文本；调用方负责严格解析。"""
        ...

    def describe(self) -> dict[str, Any]:
        ...


# ---------------------------------------------------------------------------
# 真实模型
# ---------------------------------------------------------------------------

SYSTEM_PROMPT = """你是数据库变更材料准备助手。你只输出 JSON，不要输出解释或 markdown 代码块。

输出字段：
{
  "sql": "变更 SQL",
  "rollback_sql": "回滚 SQL",
  "assumptions": [{"statement": "假设内容", "needs_confirmation": true}],
  "open_questions": ["待确认项"],
  "advisory_risk": "LOW | MEDIUM | HIGH | UNKNOWN",
  "advice_summary": "一句话风险说明"
}

硬性要求：
1. 只依据提供的表结构与证据，不得凭空推断字段名或索引名。
2. 缺少表结构信息时，在 open_questions 中说明，不要编造。
3. 你只能给出建议，不能声称"检查通过"——检查结果由确定性扫描产生。
4. SQL 必须包含回滚语句，并遵守检索到的规范条款。
"""


class ModelCallError(RuntimeError):
    """模型调用失败。

    `retryable` 把"值得重试的暂时故障"与"重试也不会变的失败"分开：
    权限拒绝、参数错误、请求体非法，重试多少次都是同一个结果，只会放大成本。

    `failure_type` 记录失败类型，`requests_sent` 记录该次调用实际发出的 HTTP 请求数，
    两者都要能被上报——否则"预算消耗了多少"只能靠猜。
    """

    def __init__(self, message: str, *, retryable: bool, failure_type: str, requests_sent: int = 0) -> None:
        super().__init__(message)
        self.retryable = retryable
        self.failure_type = failure_type
        self.requests_sent = requests_sent


# 只有这些状态码才值得重试：限流、超时与网关/服务端暂时不可用。
# 其余的 4xx（401/403 权限、400/422 参数）重试不会改变结果。
_RETRYABLE_STATUS = {408, 409, 425, 429, 500, 502, 503, 504}


class OpenAICompatibleProvider:
    """OpenAI 兼容的 Chat Completions 客户端。"""

    def __init__(self, settings: Settings) -> None:
        self._settings = settings
        self.name = "openai-compatible"
        self._semaphore = asyncio.Semaphore(4)  # 并发上限，避免打爆模型侧
        # 实际发出的 HTTP 请求数。用于如实报告预算消耗，而不是估算。
        self.requests_sent = 0

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": self._settings.llm_model}

    async def generate(self, request: DraftRequest) -> str:
        payload = {
            "model": self._settings.llm_model,
            "temperature": 0,
            "max_tokens": self._settings.llm_max_tokens,
            "messages": [
                {"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": _render_user_prompt(request)},
            ],
        }
        url = self._settings.llm_base_url + "/chat/completions"

        # 这里的重试只负责**传输层与暂时性服务端故障**。
        # 内容层面的重试（模型输出无法解析成草案）由工作流用另一个开关决定，
        # 两层不再共用同一个配置——那正是"重试次数相乘"的来源。
        attempts = max(1, int(self._settings.llm_max_attempts))
        last_error: ModelCallError | None = None
        async with self._semaphore:
            for attempt in range(attempts):
                try:
                    return await self._call_once(url, payload)
                except ModelCallError as error:
                    last_error = error
                    if not error.retryable:
                        break
                if attempt + 1 < attempts:
                    await asyncio.sleep(0.3 * (attempt + 1))
        if last_error is None:  # pragma: no cover - attempts >= 1 保证不会走到
            raise ModelCallError("模型调用失败：未发起调用", retryable=False, failure_type="not_attempted")
        # 把实际发出的请求数带上：上层需要据此报告真实消耗，而不是估算。
        last_error.requests_sent = self.requests_sent
        raise last_error

    async def _call_once(self, url: str, payload: dict[str, Any]) -> str:
        self.requests_sent += 1
        try:
            async with httpx.AsyncClient(timeout=self._settings.llm_timeout_seconds) as client:
                response = await client.post(
                    url,
                    json=payload,
                    headers={"Authorization": f"Bearer {self._settings.llm_api_key}"},
                )
        except Exception as error:  # noqa: BLE001 - 网络抖动与超时值得重试
            raise ModelCallError(
                f"模型调用失败（传输层）：{type(error).__name__}: {error}",
                retryable=True,
                failure_type="transport",
            ) from error

        if response.status_code != 200:
            raise ModelCallError(
                f"模型返回状态码 {response.status_code}: {response.text[:200]}",
                retryable=response.status_code in _RETRYABLE_STATUS,
                failure_type="status",
            )
        try:
            body = response.json()
        except Exception as error:  # noqa: BLE001 - 网关返回非 JSON 可能是暂时性的
            raise ModelCallError("模型返回的不是合法 JSON", retryable=True, failure_type="malformed_body") from error
        choices = body.get("choices") or []
        if not choices:
            raise ModelCallError("模型未返回任何候选结果", retryable=True, failure_type="no_choices")
        content = (choices[0].get("message") or {}).get("content") or ""
        if not content.strip():
            raise ModelCallError("模型返回了空内容", retryable=True, failure_type="empty_content")
        return content


def _render_user_prompt(request: DraftRequest) -> str:
    parts = [
        f"原始需求：\n{request.requirement}",
        f"已确认槽位：{request.slots.model_dump_json(exclude_none=True)}",
    ]
    if request.schema_snapshot.strip():
        parts.append(f"<untrusted source=\"schema_snapshot\">\n{request.schema_snapshot}\n</untrusted>")
    if request.evidence:
        rendered = "\n".join(
            f"- [{item.evidence_id}] {item.title} / {item.section or '-'}：{item.snippet}" for item in request.evidence
        )
        parts.append(f"<untrusted source=\"retrieved_norms\">\n{rendered}\n</untrusted>")
    if request.check_feedback:
        parts.append("上一轮确定性检查发现（必须处理或说明理由）：\n" + "\n".join(f"- {item}" for item in request.check_feedback))
    if request.previous_draft:
        parts.append(f"上一版草案：\n{request.previous_draft}")
    return "\n\n".join(parts)


# ---------------------------------------------------------------------------
# 离线确定性兜底
# ---------------------------------------------------------------------------

_IDENTIFIER = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


class DeterministicProvider:
    """不依赖模型的草案生成器。

    它不是"更聪明"的实现，而是保证模型不可用时系统仍然可用、行为可预测、
    结果可复现。生成结果同样会被上层的严格解析与确定性扫描检查。
    """

    name = "deterministic"

    def describe(self) -> dict[str, Any]:
        return {"provider": self.name, "model": "rule-based"}

    async def generate(self, request: DraftRequest) -> str:
        slots = request.slots
        # 本生成器只会产出 PostgreSQL 语法（CONCURRENTLY、lock_timeout）。
        # 工作流在入口就会拦下不支持的方言；这里再挡一道，是为了让"给 MySQL 输出
        # PG 语法"这件事不可能从别的调用路径重新混进来。
        if slots.database is not None and slots.database != DatabaseKind.POSTGRESQL:
            raise RuntimeError(
                f"确定性生成器只实现 PostgreSQL 索引变更，无法为 {slots.database.value} 生成草案。"
            )
        table = (slots.table or "").strip() or "unknown_table"
        equality_column, sort_column = _infer_columns(slots.query_sql or "")
        index_name = _index_name(table, equality_column, sort_column)

        columns = [column for column in (equality_column, sort_column) if column]
        if columns:
            column_clause = ", ".join(f"{column} DESC" if column == sort_column else column for column in columns)
        else:
            column_clause = "<待确认列>"

        statements: list[str] = []
        # 规范 2.1：DDL 变更必须先设置 lock_timeout。
        # 第一版不自动加，等确定性检查提出后再补——这样"依据检查结果修订"是真实发生的。
        if request.check_feedback:
            statements.append("SET lock_timeout = '3s';")
        statements.append(f"CREATE INDEX CONCURRENTLY {index_name}\n  ON {table} ({column_clause});")
        sql = "\n\n".join(statements)

        rollback_sql = f"DROP INDEX CONCURRENTLY IF EXISTS {index_name};"

        assumptions: list[dict[str, Any]] = []
        if "<待确认列>" not in column_clause:
            assumptions.append(
                {
                    "statement": f"按查询形态推断索引列为 ({', '.join(columns)})，与查询的等值与排序条件一致。",
                    "needs_confirmation": True,
                }
            )
        if "热表" in request.schema_snapshot:
            # 这里**不能**写 confirmed：它表示"已获人工确认"，只能由人的动作产生。
            # 离线生成器自己把假设标成已确认，正是需要修掉的失败模式之一。
            assumptions.append(
                {
                    "statement": "该表为热表，因此使用 CREATE INDEX CONCURRENTLY。",
                    "needs_confirmation": True,
                }
            )

        open_questions = [
            "请确认执行窗口是否落在低峰时段（规范要求明确到带时区的时刻）。",
            "新索引生效后是否保留原有的单列索引，需要依据使用情况另行评估。",
        ]
        if "<待确认列>" in column_clause:
            open_questions.insert(0, "缺少可解析的查询 SQL，索引列无法确认。")

        evidence_ids = [item.evidence_id for item in request.evidence]
        payload = {
            "sql": sql,
            "rollback_sql": rollback_sql,
            "assumptions": assumptions,
            "open_questions": open_questions,
            "advisory_risk": "MEDIUM" if "<待确认列>" in column_clause else "LOW",
            "advice_summary": (
                "索引列依据查询形态推断；已使用并发建索引避免长时间持锁。"
                if "<待确认列>" not in column_clause
                else "缺少查询 SQL，草案不完整，需要人工补充。"
            ),
            "evidence_ids": evidence_ids,
        }
        return json.dumps(payload, ensure_ascii=False)


def _infer_columns(query_sql: str) -> tuple[str | None, str | None]:
    """从查询 SQL 里推断"等值列"和"排序列"。

    刻意保持简单：只做最左前缀能覆盖的形态，不做 SQL 语义分析。
    解析不出来就返回 (None, None)，让上层把它变成"待确认项"，而不是猜一个列名。
    """
    if not query_sql.strip():
        return None, None

    equality: str | None = None
    sort: str | None = None

    where_match = re.search(r"(?is)\bwhere\b(.*?)(?:\border\s+by\b|\blimit\b|$)", query_sql)
    if where_match:
        for column, operator, _ in re.findall(
            r"(?i)\b([A-Za-z_][A-Za-z0-9_]*)\s*(=|>=|<=|>|<)\s*(\$\d+|'[^']*'|\d+)", where_match.group(1)
        ):
            if operator == "=" and equality is None:
                equality = column.lower()
    order_match = re.search(r"(?is)\border\s+by\s+([A-Za-z_][A-Za-z0-9_]*)\s*(asc|desc)?", query_sql)
    if order_match:
        sort = order_match.group(1).lower()

    if equality and sort and equality == sort:
        sort = None
    if not equality and not sort:
        found = _IDENTIFIER.search(query_sql)
        return (found.group(0).lower() if found else None), None
    return equality, sort


def _index_name(table: str, equality: str | None, sort: str | None) -> str:
    parts = [part for part in (table, equality, sort) if part]
    cleaned = "_".join(_sanitize(part) for part in parts)
    return f"idx_{cleaned}"[:63]


def _sanitize(value: str) -> str:
    return re.sub(r"[^a-z0-9_]", "_", value.lower())


def build_provider(settings: Settings) -> DraftProvider:
    """按配置选择实现。未配置模型时返回确定性兜底，这是可运行状态。"""
    if settings.llm_configured:
        return OpenAICompatibleProvider(settings)
    return DeterministicProvider()
