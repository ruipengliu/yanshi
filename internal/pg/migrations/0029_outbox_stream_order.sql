-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_stream_order ON outbox (business_line, xid, id);
