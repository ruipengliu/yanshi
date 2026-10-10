-- 业务线合计行分片（docs/design/m4-quota-usage.md §4）：同一业务线的每次记录都累加 end_user = '' 的合计行，
-- 高并发时成为行锁热点。合计行按用量 ID 的散列分到 shard 0..15，读取时求和；EndUser 行与匿名行只用 shard 0。
-- 已有的行 shard 为 0，含义不变。
ALTER TABLE usage_hourly ADD COLUMN shard smallint NOT NULL DEFAULT 0;
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_pkey;
ALTER TABLE usage_hourly ADD PRIMARY KEY (business_line, end_user, hour, shard);
