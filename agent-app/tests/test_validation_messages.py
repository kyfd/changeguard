"""请求校验错误的呈现。

两条口径被这里锁住：

1. **是一句人话**，不是 pydantic 的英文原文，也不是一整坨 JSON；
2. **不回显提交内容** —— pydantic 的校验错误自带 `input`（用户原文），
   把它拼进消息等于把可能要保密的正文又抄一遍送回去。
"""

from __future__ import annotations

from typing import Any

from app.main import describe_request_error


def test_over_long_requirement_says_the_limit_and_the_size() -> None:
    errors: list[dict[str, Any]] = [
        {
            "type": "string_too_long",
            "loc": ("body", "requirement"),
            "msg": "String should have at most 4000 characters",
            "input": "长" * 4001,
            "ctx": {"max_length": 4000},
        }
    ]

    message = describe_request_error(errors)

    assert "需求" in message
    assert "4000" in message and "4001" in message
    assert "长" * 20 not in message, "错误消息回显了提交内容"


def test_missing_and_enum_and_datetime_are_labelled() -> None:
    missing = describe_request_error([{"type": "missing", "loc": ("body", "requirement"), "msg": "Field required"}])
    assert missing == "缺少必填字段：需求。"

    enum = describe_request_error(
        [{"type": "enum", "loc": ("body", "database"), "msg": "Input should be 'postgresql'...", "input": "oracle"}]
    )
    assert "数据库类型" in enum
    assert "oracle" not in enum, "错误消息回显了提交内容"

    parsed = describe_request_error(
        [{"type": "datetime_parsing", "loc": ("body", "planned_at"), "msg": "Input should be a valid datetime"}]
    )
    assert "计划时间" in parsed


def test_unknown_field_names_fall_back_to_the_raw_name() -> None:
    message = describe_request_error([{"type": "value_error", "loc": ("body", "something_new"), "msg": "boom"}])
    assert "something_new" in message


def test_multiple_errors_are_summarised_not_dumped() -> None:
    errors: list[dict[str, Any]] = [
        {"type": "missing", "loc": ("body", "requirement"), "msg": "Field required"},
        {"type": "enum", "loc": ("body", "database"), "msg": "bad", "input": "oracle"},
    ]

    message = describe_request_error(errors)

    assert message.endswith("（共 2 处校验问题）")
    assert len(message) < 120, f"错误消息应当短，不要退化成转储：{message!r}"


def test_empty_error_list_is_still_a_sentence() -> None:
    assert describe_request_error([]) == "请求格式不合法。"
