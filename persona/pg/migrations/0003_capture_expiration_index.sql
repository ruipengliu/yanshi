-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS business_days_expiring ON business_days (expires_at, business_line, end_user) WHERE NOT capture_cleanup_done;
