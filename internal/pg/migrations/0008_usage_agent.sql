-- 用量记录发起调用的 AgentDef 版本（name@version），按版本比较灰度成本（docs/design/m4-agent-rollout.md §5）。
ALTER TABLE usage_entries ADD COLUMN agent text NOT NULL DEFAULT '';
