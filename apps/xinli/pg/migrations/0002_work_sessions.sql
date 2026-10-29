-- 重建仅重置执行身份，不改变业务来源或 input_id；同一 input_id 在不同 Session 中隔离。
ALTER TABLE orchestration_tasks ADD COLUMN retirements integer NOT NULL DEFAULT 0;
ALTER TABLE orchestration_tasks ADD COLUMN command_started_at timestamptz;
CREATE TABLE session_creations (
    id text PRIMARY KEY,
    business_line text NOT NULL,
    end_user text NOT NULL,
    task_id text NOT NULL REFERENCES orchestration_tasks(task_id),
    created_at timestamptz NOT NULL,
    scan_at timestamptz NOT NULL,
    payload jsonb NOT NULL
);
CREATE INDEX session_creations_owner ON session_creations(business_line,end_user);
CREATE INDEX session_creations_age ON session_creations(scan_at,id);
