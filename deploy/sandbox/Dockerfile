# yanshi 代码解释器沙箱镜像（docs/design/m2-sandbox.md）。只包含不可信代码的运行环境，不含任何平台组件（ADR-0008）。
FROM python:3.12-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ffmpeg fonts-noto-cjk \
 && rm -rf /var/lib/apt/lists/*

RUN pip install --no-cache-dir \
      numpy==2.1.3 pandas==2.2.3 matplotlib==3.9.2 openpyxl==3.1.5

# 沙箱用户与工作区；新建的命名卷会继承此目录的属主。
RUN useradd --uid 1000 --create-home --home-dir /workspace sandbox
USER 1000
WORKDIR /workspace
