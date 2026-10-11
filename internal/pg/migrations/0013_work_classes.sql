-- 工作队列的类别（ADR-0029）：业务线与优先级。认领先取优先级高的，同一优先级内按入队顺序。
-- 已有的项按交互优先级处理。
ALTER TABLE work_items ADD COLUMN business_line text NOT NULL DEFAULT '', ADD COLUMN priority smallint NOT NULL DEFAULT 1;
ALTER TABLE sandbox_items ADD COLUMN business_line text NOT NULL DEFAULT '', ADD COLUMN priority smallint NOT NULL DEFAULT 1;
ALTER TABLE janitor_items ADD COLUMN business_line text NOT NULL DEFAULT '', ADD COLUMN priority smallint NOT NULL DEFAULT 1;
CREATE INDEX work_items_claim ON work_items (priority DESC, ord);
CREATE INDEX sandbox_items_claim ON sandbox_items (priority DESC, ord);
CREATE INDEX janitor_items_claim ON janitor_items (priority DESC, ord);
DROP INDEX work_items_ord;
DROP INDEX sandbox_items_ord_idx;
DROP INDEX janitor_items_ord_idx;
