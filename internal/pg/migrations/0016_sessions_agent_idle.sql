-- yanshi:no-transaction
-- 保留策略按 AgentDef 覆盖（docs/design/captures.md §8）：Janitor 按业务线与 AgentDef 查询空闲的 Session。
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_agent_idle ON sessions (business_line, agent, last_input_at) WHERE closed_at IS NULL;
