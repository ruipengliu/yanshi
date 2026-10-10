-- 在场（docs/design/m3-duplex-channel.md §6）：哪些设备正在看某个 Session。易失、带有效期，
-- 属于 Session 数据，由 Janitor 按 Session 删除（ADR-0015）。
CREATE TABLE presence (
    session_id   text        NOT NULL,
    conn_id      text        NOT NULL,
    device_id    text        NOT NULL,
    label        text        NOT NULL,
    kind         text        NOT NULL,
    focused      boolean     NOT NULL,
    typing_until timestamptz,
    expires_at   timestamptz NOT NULL,
    PRIMARY KEY (session_id, conn_id)
);
-- 续期按连接进行（Touch）；清理按有效期进行（Prune）。
CREATE INDEX presence_conn ON presence (conn_id);
CREATE INDEX presence_expires ON presence (expires_at);

-- 每个 Session 在场内容的版本号：内容变化时递增并 NOTIFY，等待者据此判断是否需要重读。
CREATE TABLE presence_versions (
    session_id text   PRIMARY KEY,
    version    bigint NOT NULL
);
