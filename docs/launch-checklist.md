# 上线检查清单

上线（对真实 EndUser 开放）之前必须逐项完成。每项完成时注明日期与依据（提交、文档或评测报告）。新增"上线前必须替换或确认"的事项时，加到这里。

## 必须完成

- [ ] **内容安全接入真实提供商**（[设计](./design/m4-moderation.md)，ADR-0021）。当前 `serve -moderation mock` 只是关键词模拟，没有真实识别能力。接入时：
  - 实现 `moderation.Moderator`（文本与图片），按提供商的错误语义区分"违规"与"不可用"；
  - 确认流式草稿的处理是否满足监管要求，必要时实现流式检查（按片段检查、违规时取消生成）；
  - 评估是否需要对外部来源（MCP、网页）的调用结果单独检查；
  - 上线配置中 `yanshi_moderation_mock` 必须为 0。
- [ ] **价格表填入合同价**（[设计](./design/m4-quota-usage.md)）。`pricing.yaml` 中 tokenhub 的价格与沙箱单价是占位值。
- [ ] **生产沙箱**（ADR-0010，[设计](./design/m2-sandbox.md)）。当前只有 Docker 实现；选定云厂商服务或自托管隔离方案后实现生产 Provider。
- [ ] **业务线配置**：每条业务线的密钥、保留策略与配额已按约定填写（`businesslines/*.yaml`）。
- [ ] **健康信息的单独同意**（ADR-0022）：开启 `memory.health` 的业务线须在产品中取得用户的单独同意，并能出示同意记录；未取得的不得开启。
- [ ] **评测基线**：上线所用的 AgentDef 版本与模型通过 `make eval`，基线随代码提交（ADR-0018）。

## 建议确认

- [ ] 推送唤醒接入真实推送通道（M1 中仅为桩）。
- [ ] 监控告警覆盖：`yanshi_moderation_checks_total{result="error"}`、`yanshi_usage_record_errors_total`、`yanshi_release_reload_errors_total`、Run 失败率（按 `agent` 版本）。
