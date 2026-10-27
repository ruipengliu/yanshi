// Package xinli 承载心力球的产品逻辑：采集识别、账号设备、权益、渠道、推送、跨 Session 编排与 BFF。
// 知识的语义交给 Persona，Agent 的执行交给内核。
//
// 为使产品演进不改变执行内核，本模块依赖 Persona，只经公开契约（proto、gen、sdk、HTTP API、MCP）
// 使用内核，不导入 yanshi/internal；内核不导入应用层（ADR-0043）。
// 当前仅建立模块边界，设计见仓库 docs/design/persona-architecture.md。
package xinli
