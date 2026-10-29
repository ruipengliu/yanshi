-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS tasks_owner_sequence ON orchestration_tasks (business_line, end_user, sequence);
