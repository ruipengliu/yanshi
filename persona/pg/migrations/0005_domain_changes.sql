-- 接受事实的版本独立于正文保存；擦除后仍须递增，不能由 FiledAt 或当前 Day 数量推断。
CREATE TABLE accepted_facts (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    fact_key bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, fact_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

-- 变化只有标识与时间，没有正文或调用策略；与事实共用 owner 锁和提交点。
CREATE TABLE domain_changes (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    change_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL CHECK (status IN ('pending', 'acknowledged')),
    PRIMARY KEY (business_line, end_user, change_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX domain_changes_ordered ON domain_changes (business_line, end_user, ordinal);
-- 新建空表可在迁移事务内建索引；发现只访问未确认项，不扫描账本 JSON。
CREATE INDEX domain_changes_pending ON domain_changes (business_line, end_user)
    WHERE status = 'pending';
