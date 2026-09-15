"""提示注入检测。

定位：**纵深防御，不是主要控制。**

主要控制是——无论输入里写了什么，Agent 都只能调用只读工具，
检查结果由确定性扫描产生，最终由人确认材料。注入即使骗过了模型，
也改变不了这三条。

但注入在"写路径"上后果严重，所以在入口处显式检测并**停止**，
而不是继续生成一份看起来正常的草案。
"""

from __future__ import annotations

import re

_PATTERNS: list[tuple[str, re.Pattern[str]]] = [
    ("ignore-instructions", re.compile(r"(?i)ignore\s+(all\s+)?(previous|above|prior)\s+instructions")),
    ("disregard-instructions", re.compile(r"(?i)disregard\s+(all\s+)?(previous|above|prior)\s+(instructions|rules)")),
    ("reveal-system-prompt", re.compile(r"(?i)(reveal|show|print|repeat)\s+(your\s+)?(system\s+)?prompt")),
    ("role-override", re.compile(r"(?i)you\s+are\s+now\s+(a|an|the)\s+")),
    ("developer-mode", re.compile(r"(?i)(developer|debug|jailbreak)\s+mode")),
    ("cn-ignore", re.compile(r"忽略(以上|上面|之前|前面)的?(所有)?(指令|要求|规则|提示)")),
    ("cn-reveal", re.compile(r"(输出|显示|打印|告诉我)(你的)?(系统)?(提示词|prompt|指令)")),
    ("cn-role", re.compile(r"你现在是|从现在开始你")),
    ("cn-bypass", re.compile(r"(跳过|绕过|无视)(审批|检查|规范|流程)")),
]


def detect_injection(text: str) -> list[str]:
    """返回命中的规则名；空列表表示未发现。"""
    if not text or not text.strip():
        return []
    return [name for name, pattern in _PATTERNS if pattern.search(text)]


def wrap_untrusted(source: str, content: str) -> str:
    """把外部文本标记为数据而不是指令。"""
    return f'<untrusted source="{source}">\n{content}\n</untrusted>'
