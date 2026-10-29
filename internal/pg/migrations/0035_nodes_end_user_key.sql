-- yanshi:no-transaction
CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS nodes_end_user_node ON nodes (end_user, node_id);
