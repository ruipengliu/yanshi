-- Memory 的时效（docs/design/m4-memory-grant.md §11，ADR-0038）：被取代的标记失效而不删除，计划可以有有效期。
-- 只加可空或带默认值的列，不重写表。
ALTER TABLE memories ADD COLUMN superseded_by text NOT NULL DEFAULT '';
ALTER TABLE memories ADD COLUMN superseded_at timestamptz;
ALTER TABLE memories ADD COLUMN valid_until timestamptz;
