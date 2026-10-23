-- yanshi:no-transaction
-- 保留策略按 AgentDef 覆盖（docs/design/captures.md §8）：Janitor 按业务线与 AgentDef 查询已关闭、到期删除的 Session。
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_agent_closed ON sessions (business_line, agent, closed_at) WHERE closed_at IS NOT NULL;
