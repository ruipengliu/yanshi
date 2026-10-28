-- yanshi:no-transaction
CREATE INDEX CONCURRENTLY IF NOT EXISTS work_dues_due ON work_dues (due_at, session_id) INCLUDE (ord);
