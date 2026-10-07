"""构建身份（只读）。

Go 核心服务用 `internal/buildinfo` 把发布身份编进二进制。Agent 是容器镜像，
身份由部署方通过环境变量注入；未注入时**如实报告 unknown**，不按"看起来像
同一个版本"猜测，也不默认通过——`provenance_verified` 只表示四项注入字段都
通过格式校验，不表示该镜像已被评测或验收。

这里不读取任何业务数据，也不需要身份或凭据：它只报告进程自己的构建标识。
"""

from __future__ import annotations

import datetime
import os
import re
from dataclasses import dataclass
ENV_VERSION = "AGENT_BUILD_VERSION"
ENV_COMMIT = "AGENT_BUILD_COMMIT"
ENV_SOURCE_SHA256 = "AGENT_BUILD_SOURCE_SHA256"
ENV_BUILT_AT = "AGENT_BUILD_BUILT_AT"

UNKNOWN = "unknown"
# 这三种都不是"可核对的版本"：空 / unknown 表示没注入，dev 表示开发构建。
_UNSET = {"", UNKNOWN, "dev"}


def _text(name: str, *, lowercase: bool = False) -> str:
    """读取一个身份字段；缺失或空白一律返回 unknown，不用空串冒充已配置。"""
    raw = (os.getenv(name) or "").strip()
    if not raw:
        return UNKNOWN
    return raw.lower() if lowercase else raw


def _is_hex(value: str, minimum: int, maximum: int) -> bool:
    if not minimum <= len(value) <= maximum:
        return False
    return all(character in "0123456789abcdef" for character in value)


def _is_rfc3339(value: str) -> bool:
    """构建时间必须是带时区的 RFC3339；仅日期、无时区或乱写都不算发布身份。"""
    if value == UNKNOWN:
        return False
    if not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})", value):
        return False
    try:
        parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return False
    # 无时区的时间戳不能与发布时间对账。
    return parsed.tzinfo is not None


@dataclass(frozen=True)
class BuildInfo:
    version: str
    commit: str
    source_sha256: str
    built_at: str
    provenance_verified: bool

    @property
    def identified(self) -> bool:
        """版本是否**可识别**。

        unknown/dev 都不是可比较的版本：前者表示没注入，后者表示开发构建。
        两者都不能用来核对"核心与 Agent 是否配套"。
        """
        # 乱写的字符串（latest、v3.1.4 等）如实透出，但不能冒充可核对的版本。
        return bool(re.fullmatch(r"\d+\.\d+\.\d+", self.version.replace("-", ".")))

    def as_dict(self) -> dict[str, object]:
        return {
            "version": self.version,
            "commit": self.commit,
            "source_sha256": self.source_sha256,
            "built_at": self.built_at,
            "provenance_verified": self.provenance_verified,
        }


def current() -> BuildInfo:
    version = _text(ENV_VERSION)
    commit = _text(ENV_COMMIT, lowercase=True)
    source_sha256 = _text(ENV_SOURCE_SHA256, lowercase=True)
    built_at = _text(ENV_BUILT_AT)
    draft = BuildInfo(
        version=version,
        commit=commit,
        source_sha256=source_sha256,
        built_at=built_at,
        provenance_verified=False,
    )
    return BuildInfo(
        version=version,
        commit=commit,
        source_sha256=source_sha256,
        built_at=built_at,
        # 四项都要像发布身份才为真；任何一项缺失都保持 false，不"部分可信"。
        # 版本必须通过 identified 同一校验：latest 等乱写字符串即使其余三项
        # 合法，也不能冒充发布身份。
        provenance_verified=(
            draft.identified
            and _is_hex(commit, 7, 64)
            and _is_hex(source_sha256, 64, 64)
            and _is_rfc3339(built_at)
        ),
    )
    return build
