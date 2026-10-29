-- 每个物理分片一份，不含 Session 内容。恢复数据库后须显式换世代再开放消费。
CREATE TABLE outbox_stream (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    stream_epoch text NOT NULL DEFAULT gen_random_uuid()::text,
    cursor_key text NOT NULL DEFAULT gen_random_uuid()::text,
    floor xid8 NOT NULL DEFAULT '0',
    prune_target xid8 NOT NULL DEFAULT '0'
);
INSERT INTO outbox_stream (singleton) VALUES (true);

-- 同一水位只保存首次观察时间；Janitor 停机只会延长保留。
CREATE TABLE outbox_checkpoints (
    safe_xmin xid8 PRIMARY KEY,
    observed_at timestamptz NOT NULL
);
CREATE INDEX outbox_checkpoints_observed ON outbox_checkpoints (observed_at);
