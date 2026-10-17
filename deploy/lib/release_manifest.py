"""发布包组件清单校验（4.0）。

4.0 起一个发布包必须声明它携带的组件（core + agent），因为此前升级只换核心
二进制，Python Agent 会被落在旧版本上。本模块是这条规则的权威实现：预检脚本
导入它，回归测试直接驱动它。

规则（失败关闭）：
- schema 必须是受支持的版本；未知 schema 一律拒绝。
- v2 包没有组件清单：只有操作者显式声明 `--core-only` 才接受。
- v3 包必须含 `core`；除非显式 `core_only`，否则必须同时含 `agent`。
- 组件名唯一、非空、无路径穿越；摘要必须是 64 位十六进制。
"""

from __future__ import annotations

import hashlib
import os
import re
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Iterable, Mapping

SCHEMA_V3 = "changeguard-release/v3"
SCHEMA_V2 = "changeguard-core-release/v2"
SUPPORTED_SCHEMAS = (SCHEMA_V3, SCHEMA_V2)

CORE_COMPONENT = "core"
AGENT_COMPONENT = "agent"
DIGEST_PATTERN = re.compile(r"^[0-9a-f]{64}$")

#: 预检失败时使用的机器可读原因（与脚本既有 archive_error 前缀保持一致）。
ERROR_SCHEMA_INVALID = "schema_invalid"
ERROR_COMPONENTS_MISSING = "components_missing"
ERROR_COMPONENT_ENTRY_INVALID = "component_entry_invalid"
ERROR_COMPONENT_NAME_INVALID = "component_name_invalid"
ERROR_COMPONENT_DUPLICATE = "component_duplicate"
ERROR_COMPONENT_PATH_INVALID = "component_path_invalid"
ERROR_COMPONENT_DIGEST_INVALID = "component_digest_invalid"
ERROR_CORE_COMPONENT_MISSING = "core_component_missing"
ERROR_AGENT_COMPONENT_MISSING = "agent_component_missing"


@dataclass(frozen=True)
class Component:
    name: str
    path: str
    sha256: str


def validate_components(
    document: Mapping[str, Any],
    *,
    core_only: bool = False,
    required: Iterable[str] = (CORE_COMPONENT, AGENT_COMPONENT),
) -> tuple[tuple[Component, ...], str | None]:
    """按包清单返回 (components, error)。error 为 None 表示通过。

    `core_only=True` 是操作者的显式声明：它允许 v2 包与"核心-only"的 v3 包，
    但绝不会让一个缺组件的包被当成完整包。
    """
    schema = document.get("schema")
    if schema not in SUPPORTED_SCHEMAS:
        return (), ERROR_SCHEMA_INVALID

    raw = document.get("components")
    if raw is None:
        if schema == SCHEMA_V2 and core_only:
            return (Component(CORE_COMPONENT, "dbguard", ""),), None
        return (), ERROR_COMPONENTS_MISSING
    if not isinstance(raw, list) or not raw:
        return (), ERROR_COMPONENTS_MISSING

    components: list[Component] = []
    seen: set[str] = set()
    for entry in raw:
        if not isinstance(entry, Mapping):
            return (), ERROR_COMPONENT_ENTRY_INVALID
        name = entry.get("name")
        path = entry.get("path")
        digest = entry.get("sha256")
        if not isinstance(name, str) or not name.strip():
            return (), ERROR_COMPONENT_NAME_INVALID
        name = name.strip()
        if name in seen:
            return (), ERROR_COMPONENT_DUPLICATE
        seen.add(name)
        if not isinstance(path, str) or not path.strip() or ".." in path.split("/") or path.startswith("/"):
            return (), ERROR_COMPONENT_PATH_INVALID
        if not isinstance(digest, str) or not DIGEST_PATTERN.match(digest.strip().lower()):
            return (), ERROR_COMPONENT_DIGEST_INVALID
        components.append(Component(name, path.strip(), digest.strip().lower()))

    names = {component.name for component in components}
    expected = tuple(required) if not core_only else (CORE_COMPONENT,)
    for name in expected:
        if name not in names:
            return (), (
                ERROR_AGENT_COMPONENT_MISSING if name == AGENT_COMPONENT else ERROR_CORE_COMPONENT_MISSING
            )
    return tuple(components), None


def component_summary(components: Iterable[Component]) -> str:
    """稳定的 key=value 汇总，供预检脚本解析（不含任何值以外信息）。"""
    return ",".join(f"{component.name}:{component.sha256}" for component in components)


def tree_digest(directory: str | os.PathLike[str], *, root: str | os.PathLike[str] | None = None) -> str:
    """组件目录的确定性摘要：按相对路径排序后逐文件 sha256 再汇总。

    构建端（写入 manifest）与安装端（核对已安装内容）必须调用同一实现，
    否则"组件核对"退化成两个各自成立的假校验。
    """
    base = Path(directory)
    anchor = Path(root) if root is not None else base.parent
    digest = hashlib.sha256()
    entries = sorted(
        (path.relative_to(anchor).as_posix(), path) for path in base.rglob("*") if path.is_file()
    )
    for relative, path in entries:
        digest.update(f"{hashlib.sha256(path.read_bytes()).hexdigest()}  {relative}\n".encode())
    return digest.hexdigest()
