"""Pydantic AI 的参考适配。

平台的工具声明为外部（deferred）工具：模型要调用它们时 Agent 的运行以 DeferredToolRequests 结束，调用交给平台执行，
平台在下一步的 messages 中带回结果。每一步新建 Agent，从请求的 messages 重建 message_history，不用 state。

系统指令以 instructions 交给 Agent：平台的指令只来自请求的 system，Agent 自己不另加。
"""

from __future__ import annotations

from typing import Any

from pydantic_ai import Agent, DeferredToolRequests
from pydantic_ai.messages import (
    BinaryContent,
    ModelMessage,
    ModelRequest,
    ModelResponse,
    TextPart,
    ToolCallPart,
    ToolReturnPart,
    UserPromptPart,
)
from pydantic_ai.models.openai import OpenAIChatModel
from pydantic_ai.providers.openai import OpenAIProvider
from pydantic_ai.tools import ToolDefinition
from pydantic_ai.toolsets import ExternalToolset

from .server import ModelConfig
from .wire import Message, Reply, Step


def to_history(step: Step) -> list[ModelMessage]:
    """平台的 messages → message_history。连续的 user 与 tool 消息合成一个 ModelRequest：同一步多个并行调用的结果，
    以及结果之后到达的插话（不能假定最后一条是调用结果，ADR-0062 第 5 条）。"""
    names = {c.call_id: c.name for m in step.messages if m.role == "assistant" for c in m.tool_calls}
    history: list[ModelMessage] = []
    pending: list[Any] = []

    def flush() -> None:
        if pending:
            history.append(ModelRequest(parts=list(pending)))
            pending.clear()

    for m in step.messages:
        if m.role == "user":
            pending.append(UserPromptPart(content=_user_content(m)))
        elif m.role == "tool":
            pending.append(ToolReturnPart(tool_name=names.get(m.tool_call_id, ""), content=m.text(), tool_call_id=m.tool_call_id))
        elif m.role == "assistant":
            flush()
            parts: list[Any] = [TextPart(content=m.text())] if m.text() else []
            parts += [ToolCallPart(tool_name=c.name, args=c.arguments_json, tool_call_id=c.call_id) for c in m.tool_calls]
            history.append(ModelResponse(parts=parts or [TextPart(content="")]))
        else:
            raise ValueError("unknown role")
    flush()
    return history


def _user_content(m: Message) -> list[Any]:
    out: list[Any] = []
    for b in m.content:
        if b.text is not None:
            out.append(b.text)
        elif b.mime_type.startswith("image/"):
            out.append(BinaryContent(data=b.data, media_type=b.mime_type))
    return out


async def decide(step: Step, cfg: ModelConfig) -> Reply:
    model_name, api_key = cfg.for_step(step)
    provider = OpenAIProvider(base_url=cfg.base_url, api_key=api_key)
    # 不在客户端重试：内核端点已经按提供商规则重试过；配额错误更不应在这一步内重试（ADR-0063 第 4 条）。
    provider.client.max_retries = 0
    toolset = ExternalToolset(
        [ToolDefinition(name=t.name, description=t.description, parameters_json_schema=t.input_schema) for t in step.tools]
    )
    agent: Agent[None, str | DeferredToolRequests] = Agent(
        OpenAIChatModel(model_name, provider=provider),
        output_type=[str, DeferredToolRequests],
        instructions=step.system or None,
        toolsets=[toolset],
    )
    result = await agent.run(message_history=to_history(step))
    if isinstance(result.output, DeferredToolRequests):
        text = ""
        for msg in reversed(result.new_messages()):
            if isinstance(msg, ModelResponse):
                text = "".join(p.content for p in msg.parts if isinstance(p, TextPart))
                break
        return Reply(text=text, tool_calls=[(c.tool_name, c.args_as_json_str()) for c in result.output.calls])
    return Reply(text=str(result.output))
