-- Session 投影快照（session.Snapshots）：加速加载的缓存，不是事实源；随 Session 删除（ADR-0015）。
CREATE TABLE session_snapshots (
    session_id text        PRIMARY KEY,
    seq        bigint      NOT NULL,
    data       bytea       NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
