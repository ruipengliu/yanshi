-- 屏障独立于业务数据，永久保留；owners 空键行继续充当同 owner 的事务锁。
-- 新表的部分索引与建表同事务创建，无需对既有大表加锁。
CREATE TABLE owner_deletions (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'completed')),
    started_at timestamptz NOT NULL,
    completed_at timestamptz,
    PRIMARY KEY (business_line, end_user),
    CHECK ((status = 'completed') = (completed_at IS NOT NULL))
);
CREATE INDEX owner_deletions_pending ON owner_deletions (business_line, end_user)
    WHERE status = 'pending';
