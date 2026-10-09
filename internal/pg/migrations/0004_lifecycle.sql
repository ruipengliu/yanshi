-- Session 生命周期与删除（docs/design/m2-session-lifecycle.md，ADR-0015）。

-- Session 索引：日志的投影，可由日志重建。
CREATE TABLE sessions (
    session_id    text        PRIMARY KEY,
    business_line text        NOT NULL,
    end_user      text        NOT NULL,
    agent         text        NOT NULL,
    created_at    timestamptz NOT NULL,
    last_input_at timestamptz NOT NULL,
    closed_at     timestamptz
);
CREATE INDEX sessions_user ON sessions (business_line, end_user, last_input_at DESC, session_id DESC);
CREATE INDEX sessions_line ON sessions (business_line, last_input_at DESC, session_id DESC);
CREATE INDEX sessions_idle ON sessions (business_line, last_input_at) WHERE closed_at IS NULL;
CREATE INDEX sessions_closed ON sessions (business_line, closed_at) WHERE closed_at IS NOT NULL;

-- 删除记录：不含 EndUser。
CREATE TABLE deletion_requests (
    request_id    text        PRIMARY KEY,
    business_line text        NOT NULL,
    created_at    timestamptz NOT NULL
);
CREATE TABLE session_tombstones (
    session_id    text        PRIMARY KEY,
    business_line text        NOT NULL,
    reason        text        NOT NULL,
    request_id    text,
    requested_at  timestamptz NOT NULL,
    completed_at  timestamptz
);
CREATE INDEX session_tombstones_request ON session_tombstones (request_id) WHERE request_id IS NOT NULL;
CREATE INDEX session_tombstones_pending ON session_tombstones (session_id) WHERE completed_at IS NULL;

-- Janitor 的工作队列，键为 Session ID。
CREATE TABLE janitor_items (LIKE work_items INCLUDING ALL);

-- 按 Session 清理 Inbox 与去重账本；账本记录更新时间，用于按保留期清理。
ALTER TABLE inbox ADD COLUMN session_id text;
CREATE INDEX inbox_session ON inbox (session_id);
ALTER TABLE sandbox_ledger ADD COLUMN session_id text, ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX sandbox_ledger_session ON sandbox_ledger (session_id);
CREATE INDEX sandbox_ledger_updated ON sandbox_ledger (updated_at);
