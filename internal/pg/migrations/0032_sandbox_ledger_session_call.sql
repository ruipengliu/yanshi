-- yanshi:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS sandbox_ledger_session_call ON sandbox_ledger (session_id, call_id);
