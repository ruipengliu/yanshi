-- -1 保留旧消费者进度及业务线事务锁；多分片从各自的新 H 开始对账，不挪用旧游标。
ALTER TABLE outbox_recovery ADD COLUMN shard integer NOT NULL DEFAULT -1 CHECK (shard >= -1);
ALTER TABLE outbox_recovery DROP CONSTRAINT outbox_recovery_pkey;
ALTER TABLE outbox_recovery ADD PRIMARY KEY (business_line, shard);
