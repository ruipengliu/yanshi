-- 应用独立 database；来源收据冻结空映射及多目标映射，ack 丢失后不重新解释同一事实。
CREATE TABLE task_sources (
    business_line text NOT NULL,
    end_user text NOT NULL,
    source_id text NOT NULL,
    source_version bigint NOT NULL CHECK (source_version > 0),
    source_kind text NOT NULL,
    received_at timestamptz NOT NULL,
    PRIMARY KEY (business_line, end_user, source_id, source_version)
);

CREATE TABLE orchestration_tasks (
    task_id text PRIMARY KEY,
    sequence bigserial UNIQUE NOT NULL,
    business_line text NOT NULL,
    end_user text NOT NULL,
    source_id text NOT NULL,
    source_version bigint NOT NULL,
    source_kind text NOT NULL,
    operation text NOT NULL,
    target text NOT NULL,
    business_date text NOT NULL,
    input_id text UNIQUE NOT NULL,
    state text NOT NULL CHECK (state IN ('pending','unknown','accepted','succeeded','failed','terminated')),
    session_id text NOT NULL DEFAULT '',
    lease_generation bigint NOT NULL DEFAULT 0,
    lease_until timestamptz,
    retry_at timestamptz NOT NULL,
    attempt integer NOT NULL DEFAULT 0,
    receipt jsonb NOT NULL DEFAULT '{}',
    last_error_class text NOT NULL DEFAULT '',
    UNIQUE (business_line, end_user, source_id, source_version, operation, target),
    FOREIGN KEY (business_line, end_user, source_id, source_version)
        REFERENCES task_sources (business_line, end_user, source_id, source_version)
);
CREATE INDEX tasks_due ON orchestration_tasks (retry_at, sequence)
    WHERE state IN ('pending','unknown','accepted');
CREATE INDEX tasks_target ON orchestration_tasks (business_line, end_user, target, sequence)
    WHERE state IN ('pending','unknown','accepted');
CREATE INDEX tasks_owner_failures ON orchestration_tasks (business_line, end_user)
    WHERE state = 'failed';

-- 槽位不随租约失效：前任请求结果未知时，后续命令不得覆盖其执行资格。
CREATE TABLE task_sessions (
    business_line text NOT NULL,
    end_user text NOT NULL,
    session_id text NOT NULL,
    task_id text NOT NULL REFERENCES orchestration_tasks(task_id),
    PRIMARY KEY (business_line, end_user, session_id)
);
