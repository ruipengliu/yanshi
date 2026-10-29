-- yanshi:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS usage_entries_idempotency_key ON usage_entries (idempotency_key);
