-- 旧数据的 node_id 全局唯一，必然满足新约束；采用已建索引，避免切换主键时再扫描建索引。
ALTER TABLE nodes DROP CONSTRAINT nodes_pkey;
ALTER TABLE nodes ADD CONSTRAINT nodes_pkey PRIMARY KEY USING INDEX nodes_end_user_node;
ALTER TABLE inbox DROP CONSTRAINT inbox_pkey;
ALTER TABLE inbox ADD CONSTRAINT inbox_pkey PRIMARY KEY USING INDEX inbox_end_user_node_call;
ALTER TABLE inbox_versions DROP CONSTRAINT inbox_versions_pkey;
ALTER TABLE inbox_versions ADD CONSTRAINT inbox_versions_pkey PRIMARY KEY USING INDEX inbox_versions_end_user_node;
