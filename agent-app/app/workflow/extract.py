"""从需求原文里确定性抽取槽位候选值。

**这些值只是预填建议，不是已确认信息。**

三条硬边界：

1. 不调用模型：同样的输入必然得到同样的候选，可复现、可解释、可测试。
2. 抽取结果**不写入 slots**，因此不会改变 `missing()` 的判定，也不会让
   "AI 猜的" 冒充 "用户确认的"。用户提交后才成为正式槽位。
3. 宁可不给建议，也不给错建议：只在有明确书写证据时才产出候选，
   模糊表述（"今晚"、"周五晚上"）一律不猜具体时刻。
"""

from __future__ import annotations

import re

# 常见的表名书写方式：`orders` 表 / 表 orders / orders 表 / on orders
# 注意：中文「表」字两侧没有 ASCII 词边界，不能用 \b 收尾。
_TABLE_PATTERNS = (
    re.compile(r"[`\"']([A-Za-z_][\w.]{1,62})[`\"']\s*(?:这张)?表"),
    re.compile(r"([A-Za-z_][\w.]{1,62})\s*(?:这张)?表"),
    re.compile(r"(?:这张)?表\s*[`\"']?([A-Za-z_][\w.]{1,62})[`\"']?"),
    re.compile(r"(?i)\b(?:on|from|into|update|alter\s+table|table)\s+([A-Za-z_][\w.]{1,62})\b"),
)

# 环境：命中即认为是显式书写，不做同义推断。
_ENVIRONMENTS = (
    ("生产", ("生产", "线上", "prod", "production")),
    ("预发", ("预发", "预发布", "staging", "pre", "uat")),
    ("测试", ("测试", "test", "qa", "sit")),
    ("开发", ("开发", "dev", "development", "本地")),
)

_DATABASES = (
    ("postgresql", ("postgresql", "postgres", "pgsql", "pg库", " pg ")),
    ("mysql", ("mysql", "mariadb")),
)

# 应用名：形如 order-service / user_api / payment.svc，且不是纯中文。
_APPLICATION = re.compile(
    r"(?i)(?:应用|服务|系统|app|service)\s*[:：是为]?\s*[`\"']?([A-Za-z][\w.-]{2,48})[`\"']?"
)
_APPLICATION_BARE = re.compile(r"\b([a-z][a-z0-9]*(?:[-_][a-z0-9]+)+)\b")

# 明确到分钟的时刻才接受，例如 2026-09-18 21:30 / 2026/9/18 21:30。
_EXPLICIT_DATETIME = re.compile(
    r"\b(\d{4})[-/年](\d{1,2})[-/月](\d{1,2})日?[\sT]+(\d{1,2})[:：点](\d{2})\b"
)

# 这些词看起来像表名/应用名，但其实是 SQL 关键字或通用词，不能当候选。
_STOPWORDS = {
    "index", "table", "select", "insert", "delete", "create", "drop",
    "sql", "ddl", "dml", "where", "join", "null", "from", "into",
    "postgresql", "postgres", "mysql", "mariadb", "id", "db", "data",
}


def _clean(value: str | None) -> str | None:
    if not value:
        return None
    text = value.strip().strip("`\"'，。,.;；:：").strip()
    if not text or text.lower() in _STOPWORDS:
        return None
    return text


def extract_table(text: str) -> str | None:
    for pattern in _TABLE_PATTERNS:
        for match in pattern.finditer(text or ""):
            candidate = _clean(match.group(1))
            # 表名不接受带连字符的写法，避免把 order-service 这类应用名当成表。
            if candidate and "-" not in candidate:
                return candidate
    return None


def extract_environment(text: str) -> str | None:
    lowered = (text or "").lower()
    for label, markers in _ENVIRONMENTS:
        if any(marker.lower() in lowered for marker in markers):
            return label
    return None


def extract_database(text: str) -> str | None:
    lowered = f" {(text or '').lower()} "
    for label, markers in _DATABASES:
        if any(marker in lowered for marker in markers):
            return label
    return None


def extract_application(text: str) -> str | None:
    match = _APPLICATION.search(text or "")
    candidate = _clean(match.group(1)) if match else None
    if candidate:
        return candidate
    # 退一步：需求里独立出现的 kebab/snake 命名，通常就是服务名。
    for bare in _APPLICATION_BARE.finditer((text or "").lower()):
        candidate = _clean(bare.group(1))
        if candidate:
            return candidate
    return None


def extract_planned_at(text: str) -> str | None:
    """只接受写到分钟的明确时刻。

    "今晚"、"周五晚上"这类表述**不猜**：规范要求明确时刻，猜测会把
    人的决策替换成模型的默认值。
    """
    match = _EXPLICIT_DATETIME.search(text or "")
    if not match:
        return None
    year, month, day, hour, minute = (int(part) for part in match.groups())
    if not (1 <= month <= 12 and 1 <= day <= 31 and 0 <= hour <= 23 and 0 <= minute <= 59):
        return None
    # 返回 datetime-local 控件可直接使用的格式。
    return f"{year:04d}-{month:02d}-{day:02d}T{hour:02d}:{minute:02d}"


_EXTRACTORS = {
    "application": extract_application,
    "environment": extract_environment,
    "database": extract_database,
    "table": extract_table,
    "planned_at": extract_planned_at,
}


def suggest_slots(requirement: str, fields: list[str]) -> dict[str, str]:
    """为指定字段抽取候选值。抽不到的字段直接不出现在结果里。"""
    suggestions: dict[str, str] = {}
    for field in fields:
        extractor = _EXTRACTORS.get(field)
        if extractor is None:
            continue
        value = extractor(requirement or "")
        if value:
            suggestions[field] = value
    return suggestions


__all__ = ["suggest_slots"]
