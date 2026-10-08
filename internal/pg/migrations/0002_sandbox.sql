-- 沙箱队列：语义同 work_items，键为沙箱 Node ID（docs/design/m2-sandbox.md）。
CREATE TABLE sandbox_items (LIKE work_items INCLUDING ALL);

-- 沙箱控制器共享的 call_id 去重账本（ADR-0008）。result 为 protobuf 编码的 yanshi.v1.InvokeResult。
CREATE TABLE sandbox_ledger (
    call_id text     PRIMARY KEY,
    state   smallint NOT NULL,
    result  bytea
);

-- 沙箱最近活跃时间，用于空闲回收。
CREATE TABLE sandbox_activity (
    sandbox_id  text        PRIMARY KEY,
    last_active timestamptz NOT NULL
);
