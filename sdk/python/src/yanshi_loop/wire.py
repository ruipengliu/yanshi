"""外部循环一步的线上格式（proto/yanshi/v1/loop.proto）。

平台以 protojson 发送 LoopStepRequest：字段名为 lowerCamelCase（这里也接受原名），bytes 为 base64；未知字段忽略。
回答 LoopStepResponse 同样按 protojson。这里只取适配需要的字段，不依赖 protobuf 库。
"""

from __future__ import annotations

import base64
import json
import re
from dataclasses import dataclass, field
from typing import Any


def _get(d: dict[str, Any], camel: str, default: Any = None) -> Any:
    if camel in d:
        return d[camel]
    snake = re.sub(r"([A-Z])", lambda m: "_" + m.group(1).lower(), camel)
    return d.get(snake, default)


@dataclass
class Block:
    """内容块：只有文本与媒体（卡片动作、界面上下文等平台已呈现为文本）。"""

    text: str | None = None
    mime_type: str = ""
    data: bytes = b""


@dataclass
class ToolCall:
    # 平台改写后的全局唯一 ID：之后的上下文都用它，按它配对结果即可（ADR-0062 第 5 条）。
    call_id: str
    name: str
    arguments_json: str


@dataclass
class Message:
    role: str  # "user"、"assistant" 或 "tool"
    content: list[Block] = field(default_factory=list)
    tool_calls: list[ToolCall] = field(default_factory=list)
    tool_call_id: str = ""

    def text(self) -> str:
        return "".join(b.text or "" for b in self.content)


@dataclass
class Tool:
    name: str
    description: str
    input_schema: dict[str, Any]


@dataclass
class Step:
    session_id: str
    run_id: str
    attempt: int
    turn: int
    system: str
    messages: list[Message]
    tools: list[Tool]
    state: bytes
    state_version: str
    # 步令牌：调用内核模型端点的 API key（ADR-0063 第 2 条）。登记没有配置密钥时为空。
    step_token: str
    # 经内核模型端点可用的模型，默认用第一个；评测换模型时是评测的模型，不要写死（ADR-0063 第 3 条）。
    models: list[str]


def _block(d: dict[str, Any]) -> Block:
    if "text" in d:
        return Block(text=_get(d["text"], "text", ""))
    media = d.get("media")
    if media is not None:
        return Block(mime_type=_get(media, "mimeType", ""), data=base64.b64decode(_get(media, "data", "") or ""))
    return Block(text="")


def _call(d: dict[str, Any]) -> ToolCall:
    return ToolCall(
        call_id=_get(d, "callId", ""),
        name=_get(d, "capability", ""),
        arguments_json=_get(d, "argumentsJson", "") or "{}",
    )


def parse_request(body: bytes) -> Step:
    d = json.loads(body)
    messages = [
        Message(
            role=_get(m, "role", ""),
            content=[_block(b) for b in _get(m, "content", []) or []],
            tool_calls=[_call(c) for c in _get(m, "toolCalls", []) or []],
            tool_call_id=_get(m, "toolCallId", "") or "",
        )
        for m in _get(d, "messages", []) or []
    ]
    tools = [
        Tool(
            name=_get(t, "name", ""),
            description=_get(t, "description", "") or "",
            input_schema=json.loads(_get(t, "inputSchemaJson", "") or "{}"),
        )
        for t in _get(d, "tools", []) or []
    ]
    return Step(
        session_id=_get(d, "sessionId", ""),
        run_id=_get(d, "runId", ""),
        attempt=int(_get(d, "attempt", 0) or 0),
        turn=int(_get(d, "turn", 0) or 0),
        system=_get(d, "system", "") or "",
        messages=messages,
        tools=tools,
        state=base64.b64decode(_get(d, "state", "") or ""),
        state_version=_get(d, "stateVersion", "") or "",
        step_token=_get(d, "stepToken", "") or "",
        models=list(_get(d, "models", []) or []),
    )


@dataclass
class Reply:
    """一步的回答。tool_calls 为空表示 Run 完成；调用一律由平台执行（ADR-0062 第 4 条）。"""

    text: str = ""
    # (能力名, 参数 JSON)。调用 ID 由平台改写，这里不给。
    tool_calls: list[tuple[str, str]] = field(default_factory=list)
    # 不透明的状态，至多 64 KiB，下一步原样带回；Run 的第一步为空。参考适配不需要状态。
    state: bytes = b""
    state_version: str = ""


def encode_response(r: Reply) -> bytes:
    out: dict[str, Any] = {
        "content": [{"text": {"text": r.text}}] if r.text else [],
        "toolCalls": [{"capability": name, "argumentsJson": args} for name, args in r.tool_calls],
    }
    if r.state:
        out["state"] = base64.b64encode(r.state).decode()
    if r.state_version:
        out["stateVersion"] = r.state_version
    return json.dumps(out, ensure_ascii=False).encode()


def data_url(b: Block) -> str:
    return f"data:{b.mime_type};base64,{base64.b64encode(b.data).decode()}"
