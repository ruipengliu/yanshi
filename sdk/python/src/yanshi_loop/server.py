"""外部循环服务：接收一步、交给框架、回答一步。

无状态：每一步从请求重建框架的输入，不在进程里保存跨步的东西，所以换进程、同一步重复请求、attempt 递增都不需要
特殊处理；也不得按 (run_id, attempt, turn) 去重或缓存回答（ADR-0062 第 2、3 条）。
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Awaitable, Callable

from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.routing import Route

from .wire import Reply, Step, encode_response, parse_request

log = logging.getLogger("yanshi_loop")


@dataclass
class ModelConfig:
    """模型的连接方式。base_url 指向内核的模型端点（<yanshi>/v1/loop，ADR-0063），或测试时 looptest 的假模型。"""

    base_url: str
    # 请求没有给出 models 时用的模型（外部服务自带模型，ADR-0062）。
    default_model: str
    # 请求没有步令牌时用的 API key（自带模型的服务自己的凭证）。
    default_api_key: str = "unused"

    def for_step(self, step: Step) -> tuple[str, str]:
        """这一步用的 (模型, API key)：有 models 时用第一个，有步令牌时用它作 API key。"""
        model = step.models[0] if step.models else self.default_model
        return model, step.step_token or self.default_api_key


Decide = Callable[[Step, ModelConfig], Awaitable[Reply]]


def app(decide: Decide, cfg: ModelConfig) -> Starlette:
    async def handle(request: Request) -> Response:
        try:
            step = parse_request(await request.body())
        except Exception:  # noqa: BLE001 — 结构错误，不带内容
            return JSONResponse({"error": "unparsable step request"}, status_code=400)
        try:
            reply = await decide(step, cfg)
        except Exception as e:  # noqa: BLE001
            # 模型或框架失败：返回非 200，平台按模型错误处理（ADR-0062 第 4 条）。日志只记 ID，不记内容。
            log.warning("step failed: session=%s run=%s turn=%s error=%s", step.session_id, step.run_id, step.turn, type(e).__name__)
            return JSONResponse({"error": "step failed"}, status_code=502)
        return Response(encode_response(reply), media_type="application/json")

    return Starlette(routes=[Route("/", handle, methods=["POST"])])
