-- 历史 ID 就是原幂等键；匿名行也保留它，升级后的重投递不能再计一次费用。
-- 与写入方停写切换；不重写历史对象 ID，也不保存已注销 EndUser 的副本。
ALTER TABLE usage_entries ADD COLUMN idempotency_key text;
UPDATE usage_entries SET idempotency_key = id;
ALTER TABLE usage_entries ALTER COLUMN idempotency_key SET NOT NULL;
