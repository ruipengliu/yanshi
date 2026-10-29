-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS accounts_stable_order ON accounts (business_line, end_user COLLATE "C");
