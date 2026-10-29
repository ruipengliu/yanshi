-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS sessions_stable_order ON sessions (business_line, end_user, session_id COLLATE "C");
