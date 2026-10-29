-- 工作资格与历史在同一 owner 事务保存；原文不进入这些表。
CREATE TABLE work_bindings (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    target bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, target),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE TABLE work_sessions (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    session_id bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, session_id),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
