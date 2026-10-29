-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS outbox_receipts_pending ON outbox_receipts(business_line,session_id,seq) WHERE NOT processed;
