"""需求原文槽位抽取测试。

抽取只产生**预填建议**：它必须可复现、不越界，并且在证据不足时宁可不给。
"""

from __future__ import annotations

from app.workflow.extract import suggest_slots

FIELDS = ["application", "environment", "database", "table", "planned_at"]

REQUIREMENT = (
    "订单列表页最近特别慢。想给 orders 表针对按用户和创建时间的查询加一个复合索引，"
    "目标 PostgreSQL，应用是 order-service，在生产环境执行。"
)


def test_extracts_written_facts() -> None:
    result = suggest_slots(REQUIREMENT, FIELDS)
    assert result["table"] == "orders"
    assert result["environment"] == "生产"
    assert result["database"] == "postgresql"
    assert result["application"] == "order-service"


def test_vague_time_is_not_guessed() -> None:
    """「今晚」「周五晚上」不是明确时刻，绝不能替用户猜一个具体时间。"""
    for text in ("今晚低峰执行", "这周找个晚上执行", "周五晚上执行", "尽快执行"):
        assert "planned_at" not in suggest_slots(text, FIELDS)


def test_explicit_time_is_extracted() -> None:
    assert suggest_slots("计划 2026-09-18 21:30 执行", FIELDS)["planned_at"] == "2026-09-18T21:30"
    assert suggest_slots("安排在 2026年9月18日 21:30", FIELDS)["planned_at"] == "2026-09-18T21:30"


def test_invalid_time_is_rejected() -> None:
    assert "planned_at" not in suggest_slots("计划 2026-13-45 99:99 执行", FIELDS)


def test_returns_nothing_without_evidence() -> None:
    """没有可依据的书写内容时不产出任何建议，而不是编一个默认值。"""
    assert suggest_slots("帮我处理一下那个慢查询问题", ["table", "environment", "database"]) == {}


def test_only_requested_fields_are_suggested() -> None:
    result = suggest_slots(REQUIREMENT, ["table"])
    assert set(result) == {"table"}


def test_application_name_is_not_taken_as_table() -> None:
    result = suggest_slots("给 order-service 的 orders 表加索引", FIELDS)
    assert result["table"] == "orders"


def test_suggestions_do_not_fill_slots(service, context) -> None:
    """建议值不能进入 slots：缺失判定必须仍然认为这些信息没有被确认。"""
    from tests.conftest import bare_request, run

    view, slots = run(service.create_task(bare_request(requirement=REQUIREMENT), context))
    assert view.status.value == "NEEDS_INFO"
    # 槽位仍然为空，说明抽取没有替用户做确认。
    assert slots.table is None
    assert slots.application is None
    assert view.slots.table is None
    # 但追问里带上了可核对的建议值。
    suggested = {item.field: item.suggested for item in view.questions if item.suggested}
    assert suggested.get("table") == "orders"
    assert suggested.get("environment") == "生产"
    assert all(item.suggested_from == "需求原文" for item in view.questions if item.suggested)
