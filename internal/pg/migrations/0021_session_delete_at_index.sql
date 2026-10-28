-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_delete_at ON sessions (delete_at, business_line, session_id) WHERE delete_at IS NOT NULL;
