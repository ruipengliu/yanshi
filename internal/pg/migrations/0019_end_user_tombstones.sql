CREATE TABLE end_user_tombstones (
    business_line text NOT NULL,
    end_user text NOT NULL,
    request_id text NOT NULL,
    requested_at timestamptz NOT NULL,
    sealed boolean NOT NULL DEFAULT false,
    session_ids text[] NOT NULL DEFAULT '{}',
    revision bigint NOT NULL DEFAULT 0,
    completed_at timestamptz,
    expires_at timestamptz,
    PRIMARY KEY (end_user, business_line)
);

CREATE INDEX end_user_tombstones_pending ON end_user_tombstones (business_line, end_user)
    WHERE sealed AND completed_at IS NULL;
CREATE INDEX end_user_tombstones_expiry ON end_user_tombstones (expires_at)
    WHERE completed_at IS NOT NULL;
