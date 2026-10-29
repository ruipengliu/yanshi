-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_retention_order ON outbox (xid, id) INCLUDE (session_id);
