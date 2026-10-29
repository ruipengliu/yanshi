-- yanshi:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS inbox_versions_end_user_node ON inbox_versions (end_user, node_id);
