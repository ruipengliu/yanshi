-- 普通信封随 Session 清理，删除信封无 EndUser、无 Event seq，独立保留。
CREATE TABLE outbox (
    id bigserial PRIMARY KEY,
    xid xid8 NOT NULL DEFAULT pg_current_xact_id(),
    envelope_id text NOT NULL UNIQUE,
    business_line text NOT NULL,
    session_id text NOT NULL,
    seq bigint,
    kind text NOT NULL,
    end_user text,
    run_id text,
    call_id text,
    agent text,
    agent_version text,
    reason text,
    deletion_request_id text,
    UNIQUE (session_id, seq, kind),
    CHECK (
        (kind = 'session_deleted' AND reason IS NOT NULL AND seq IS NULL AND end_user IS NULL
         AND run_id IS NULL AND call_id IS NULL AND agent IS NULL AND agent_version IS NULL)
        OR (kind IN ('run_completed', 'run_failed', 'question', 'approval_requested',
                     'background_settled', 'session_closed') AND seq IS NOT NULL AND seq > 0 AND end_user IS NOT NULL
            AND reason IS NULL AND deletion_request_id IS NULL)
    )
);
CREATE INDEX outbox_business_line_order ON outbox (business_line, id);
