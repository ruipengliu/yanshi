-- 工作变为可认领的时刻（docs/design/deployment.md §7）：认领时得出排队等了多久（yanshi_queue_wait_seconds），
-- 积压统计与 Submit 准入据此估计等待。已有的项按迁移时刻计。
ALTER TABLE work_items ADD COLUMN ready_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE sandbox_items ADD COLUMN ready_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE janitor_items ADD COLUMN ready_at timestamptz NOT NULL DEFAULT now();
