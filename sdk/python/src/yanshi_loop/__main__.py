"""运行一个参考适配：python -m yanshi_loop --framework langgraph|pydantic --port 8001 --model-base-url URL"""

from __future__ import annotations

import argparse
import logging
import os

import uvicorn

from .server import ModelConfig, app


def main() -> None:
    p = argparse.ArgumentParser(prog="yanshi-loop")
    p.add_argument("--framework", choices=["langgraph", "pydantic"], required=True)
    p.add_argument("--host", default="127.0.0.1")
    p.add_argument("--port", type=int, default=8001)
    p.add_argument(
        "--model-base-url",
        default=os.environ.get("YANSHI_LOOP_MODEL_BASE_URL", "http://127.0.0.1:18472/v1/loop"),
        help="OpenAI 兼容的模型地址：内核的模型端点 <yanshi>/v1/loop（ADR-0063），或 looptest 的假模型",
    )
    p.add_argument(
        "--model",
        default=os.environ.get("YANSHI_LOOP_MODEL", ""),
        help="请求没有给出 models 时用的模型（自带模型的服务）",
    )
    args = p.parse_args()
    if args.framework == "langgraph":
        from .langgraph_loop import decide
    else:
        from .pydantic_loop import decide
    logging.basicConfig(level=logging.INFO)
    # 模型客户端逐条记录请求地址，没有用处；只留适配自己的日志（只含 ID）。
    for name in ("httpx", "httpx2", "openai"):
        logging.getLogger(name).setLevel(logging.WARNING)
    cfg = ModelConfig(base_url=args.model_base_url, default_model=args.model,
                      default_api_key=os.environ.get("YANSHI_LOOP_API_KEY", "unused"))
    uvicorn.run(app(decide, cfg), host=args.host, port=args.port, log_level="warning")


if __name__ == "__main__":
    main()
