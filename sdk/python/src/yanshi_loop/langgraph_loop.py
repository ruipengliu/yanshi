"""LangGraph 的参考适配。

一步就是图的一次运行：从请求的 messages 重建对话，agent 节点调用绑定了平台工具的模型；模型要调用工具时图直接结束，
把调用交给平台执行（外部循环只做决定，ADR-0041），平台执行后在下一步的 messages 中带回结果。

这个图不需要 LangGraph 的 checkpointer，也不用 state。需要在多个节点之间停在中途的图（例如工具存根里 interrupt()），
把 checkpoint 去掉消息后放进 Reply.state、下一步从 Step.state 恢复：消息每步都由平台给出，留在 checkpoint 里会让状态
随轮数增长并超过 64 KiB（ADR-0062 的原型结论）。图的 thread_id 只用 run_id，不拼 attempt。
"""

from __future__ import annotations

import json

from langchain_core.messages import AIMessage, BaseMessage, HumanMessage, SystemMessage, ToolMessage
from langchain_openai import ChatOpenAI
from langgraph.graph import END, START, MessagesState, StateGraph

from .server import ModelConfig
from .wire import Message, Reply, Step, data_url


def to_langchain(step: Step) -> list[BaseMessage]:
    out: list[BaseMessage] = []
    if step.system:
        out.append(SystemMessage(step.system))
    for m in step.messages:
        out.append(_message(m))
    return out


def _message(m: Message) -> BaseMessage:
    if m.role == "user":
        parts: list[str | dict] = []
        for b in m.content:
            if b.text is not None:
                parts.append({"type": "text", "text": b.text})
            elif b.mime_type.startswith("image/"):
                parts.append({"type": "image_url", "image_url": {"url": data_url(b)}})
        return HumanMessage(parts)
    if m.role == "assistant":
        return AIMessage(
            m.text(),
            tool_calls=[
                {"id": c.call_id, "name": c.name, "args": json.loads(c.arguments_json or "{}"), "type": "tool_call"}
                for c in m.tool_calls
            ],
        )
    if m.role == "tool":
        # 被拒、能力拒绝与执行失败的结果也是一条 tool 消息，内容是平台写的说明（ADR-0062 第 5 条）。
        return ToolMessage(m.text(), tool_call_id=m.tool_call_id)
    raise ValueError("unknown role")


async def decide(step: Step, cfg: ModelConfig) -> Reply:
    model_name, api_key = cfg.for_step(step)
    # 不在客户端重试：内核端点已经按提供商规则重试过；配额错误更不应在这一步内重试（ADR-0063 第 4 条）。
    llm = ChatOpenAI(model=model_name, base_url=cfg.base_url, api_key=api_key, max_retries=0)
    tools = [
        {"type": "function", "function": {"name": t.name, "description": t.description, "parameters": t.input_schema}}
        for t in step.tools
    ]
    bound = llm.bind_tools(tools) if tools else llm

    async def agent(state: MessagesState) -> dict:
        return {"messages": [await bound.ainvoke(state["messages"])]}

    graph = StateGraph(MessagesState)
    graph.add_node("agent", agent)
    graph.add_edge(START, "agent")
    graph.add_edge("agent", END)
    result = await graph.compile().ainvoke({"messages": to_langchain(step)})
    last = result["messages"][-1]
    assert isinstance(last, AIMessage)
    text = last.content if isinstance(last.content, str) else "".join(
        p.get("text", "") for p in last.content if isinstance(p, dict)
    )
    return Reply(text=text, tool_calls=[(c["name"], json.dumps(c["args"], ensure_ascii=False)) for c in last.tool_calls])
