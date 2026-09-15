"""工具注册表。

三条硬约束（与 Go 侧 Agent 保持一致的语义）：

1. **白名单**：未注册的工具名直接拒绝，不做任何"猜测执行"。
2. **参数校验**：按 JSON Schema 子集校验，未知参数直接拒绝，不静默忽略。
3. **身份不可由调用方声明**：用户/组织/应用来自 `TrustedContext`，
   由 API 层从已认证请求注入；工具参数里出现这些字段会被当作未知字段拒绝。
"""

from __future__ import annotations

import time
from dataclasses import dataclass, field
from typing import Any, Awaitable, Callable, Mapping

from app.schemas.drafts import ToolResult


class ToolError(Exception):
    """工具层错误基类。"""


class UnknownTool(ToolError):
    """工具不在白名单中。"""


class InvalidToolArgs(ToolError):
    """参数不符合 schema。"""


class ToolNotReadOnly(ToolError):
    """工具会修改数据，而 Agent 只允许只读工具。"""


@dataclass(frozen=True)
class TrustedContext:
    """可信调用上下文。**只能由已认证的 API 层构造**。"""

    user_id: str
    organization_id: str
    application_id: str | None = None

    def as_headers(self) -> dict[str, str]:
        headers = {
            "X-Actor-Id": self.user_id,
            "X-Org-Id": self.organization_id,
        }
        if self.application_id:
            headers["X-Application-Id"] = self.application_id
        return headers


ToolExecute = Callable[[TrustedContext, Mapping[str, Any]], Awaitable[ToolResult]]


@dataclass
class Tool:
    name: str
    description: str
    parameters: dict[str, Any]
    execute: ToolExecute
    read_only: bool = True


@dataclass
class ToolRegistry:
    """已注册工具的集合。"""

    _tools: dict[str, Tool] = field(default_factory=dict)

    def register(self, tool: Tool) -> None:
        self._tools[tool.name] = tool

    def specs(self) -> list[dict[str, Any]]:
        return [
            {
                "name": tool.name,
                "description": tool.description,
                "parameters": tool.parameters,
                "read_only": tool.read_only,
            }
            for tool in sorted(self._tools.values(), key=lambda item: item.name)
        ]

    def lookup(self, name: str) -> Tool | None:
        return self._tools.get(name)

    async def call(self, name: str, args: Mapping[str, Any] | None, context: TrustedContext) -> ToolResult:
        started = time.perf_counter()
        tool = self.lookup(name)
        if tool is None:
            raise UnknownTool(f"工具不在白名单中：{name}")
        if not tool.read_only:
            raise ToolNotReadOnly(f"Agent 只允许只读工具：{name}")

        payload = dict(args or {})
        validate_args(payload, tool.parameters)

        try:
            result = await tool.execute(context, payload)
        except Exception as error:  # noqa: BLE001 - 工具失败必须变成明确结果，不能是"通过"
            result = ToolResult(ok=False, tool=name, error=f"{type(error).__name__}: {error}")
        result.duration_ms = int((time.perf_counter() - started) * 1000)
        return result


def validate_args(args: Mapping[str, Any], schema: Mapping[str, Any]) -> None:
    """校验参数。语义刻意收紧：未知字段直接拒绝。"""
    properties = schema.get("properties") or {}
    for name in schema.get("required") or []:
        if name not in args:
            raise InvalidToolArgs(f"缺少必填参数 {name!r}")

    for name, value in args.items():
        if name not in properties:
            raise InvalidToolArgs(f"未知参数 {name!r}")
        expected = properties[name].get("type")
        if expected is None or value is None:
            continue
        if not _type_matches(expected, value):
            raise InvalidToolArgs(f"参数 {name!r} 期望类型 {expected}")
        if expected == "integer" and isinstance(value, int):
            minimum = properties[name].get("minimum")
            maximum = properties[name].get("maximum")
            if minimum is not None and value < minimum:
                raise InvalidToolArgs(f"参数 {name!r} 不能小于 {minimum}")
            if maximum is not None and value > maximum:
                raise InvalidToolArgs(f"参数 {name!r} 不能大于 {maximum}")


def _type_matches(expected: str, value: Any) -> bool:
    if expected == "string":
        return isinstance(value, str)
    if expected == "integer":
        return isinstance(value, int) and not isinstance(value, bool)
    if expected == "number":
        return isinstance(value, (int, float)) and not isinstance(value, bool)
    if expected == "boolean":
        return isinstance(value, bool)
    if expected == "object":
        return isinstance(value, dict)
    if expected == "array":
        return isinstance(value, list)
    return True


def object_schema(properties: dict[str, Any] | None = None, required: list[str] | None = None) -> dict[str, Any]:
    """构造一个 object schema，默认不允许额外字段。"""
    return {
        "type": "object",
        "properties": properties or {},
        "required": required or [],
        "additionalProperties": False,
    }
