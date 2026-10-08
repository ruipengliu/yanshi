-- Session Event 日志（ADR-0004、ADR-0007）。data 为 protobuf 编码的 yanshi.v1.Event。
CREATE TABLE events (
    session_id text        NOT NULL,
    seq        bigint      NOT NULL,
    data       bytea       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, seq)
);

-- 按 Session 发放租约的工作队列。时间均由应用传入（clock.Clock），不使用数据库 now()。
CREATE SEQUENCE work_seq;
CREATE TABLE work_items (
    session_id    text        PRIMARY KEY,
    leased        boolean     NOT NULL DEFAULT false,
    holder        text,
    token         bigint,
    lease_expires timestamptz,
    dirty         boolean     NOT NULL DEFAULT false,
    parked_until  timestamptz,
    ord           bigint      NOT NULL
);
CREATE INDEX work_items_ord ON work_items (ord);

-- Node 目录。capabilities 为 protobuf 编码的 yanshi.v1.CapabilitySet。
CREATE TABLE nodes (
    node_id       text        PRIMARY KEY,
    business_line text        NOT NULL,
    end_user      text        NOT NULL,
    label         text        NOT NULL,
    kind          text        NOT NULL,
    host_app      text        NOT NULL,
    capabilities  bytea       NOT NULL,
    online        boolean     NOT NULL,
    last_seen     timestamptz NOT NULL,
    gen           bigint      NOT NULL,
    UNIQUE (business_line, end_user, label)
);

-- Node Inbox。data 为 protobuf 编码的 yanshi.v1.Invoke。
CREATE SEQUENCE inbox_seq;
CREATE TABLE inbox (
    node_id text   NOT NULL,
    call_id text   NOT NULL,
    data    bytea  NOT NULL,
    ord     bigint NOT NULL,
    PRIMARY KEY (node_id, call_id)
);
CREATE TABLE inbox_versions (
    node_id text   PRIMARY KEY,
    version bigint NOT NULL
);
