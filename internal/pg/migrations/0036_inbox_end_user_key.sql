-- yanshi:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS inbox_end_user_node_call ON inbox (end_user, node_id, call_id);
