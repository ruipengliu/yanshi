// Package persona 承载关于 EndUser 的结构化知识：实体、交谈、承诺、项目、笔记等，
// 以变更集日志为事实源，按业务线隔离；不负责账号、设备、渠道、推送或 Agent 的执行。
//
// 为使业务语义独立于执行内核，本模块只经内核公开契约（proto、gen、sdk、HTTP API、MCP）
// 接入内核，不导入 yanshi/internal；应用层依赖 Persona，内核不导入 Persona（ADR-0043）。
// 当前仅建立模块边界，设计见仓库 docs/design/persona-architecture.md。
package persona
