-- 命中前缀缓存的输入 token（docs/design/m4-quota-usage.md §2）：价格通常远低于普通输入，单独计量。
ALTER TABLE usage_entries ADD COLUMN cached_input_tokens bigint NOT NULL DEFAULT 0;
