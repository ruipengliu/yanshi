-- 停写升级（含网关、Worker、控制器与 Janitor）：从旧归属恢复真实 EndUser，不从 ID 的槽位猜测。
ALTER TABLE inbox ADD COLUMN end_user text;
ALTER TABLE inbox_versions ADD COLUMN end_user text;

-- 设备含无 Session 的 forget 调用从目录恢复；沙箱与已注销设备从 Session 索引恢复。
UPDATE inbox i SET end_user = n.end_user FROM nodes n WHERE i.node_id = n.node_id;
UPDATE inbox i SET end_user = s.end_user FROM sessions s
WHERE i.end_user IS NULL AND i.session_id = s.session_id;
-- 已有目录与 Session 的归属不一致时无法安全选择接收人，须停下核实历史数据。
DO $$ BEGIN
    IF EXISTS (
        SELECT 1 FROM inbox i JOIN sessions s ON s.session_id = i.session_id
        WHERE i.end_user <> s.end_user
    ) OR EXISTS (
        SELECT 1 FROM inbox GROUP BY node_id HAVING count(DISTINCT end_user) > 1
    ) THEN
        RAISE EXCEPTION 'inbox ownership is ambiguous; verify historical owners before migration';
    END IF;
END $$;
-- 不能恢复归属的历史投递须先人工核实；失败回滚整个迁移，绝不静默丢弃调用。
ALTER TABLE inbox ALTER COLUMN end_user SET NOT NULL;

UPDATE inbox_versions v SET end_user = n.end_user FROM nodes n WHERE v.node_id = n.node_id;
UPDATE inbox_versions v SET end_user = i.end_user FROM inbox i
WHERE v.end_user IS NULL AND v.node_id = i.node_id;
UPDATE inbox_versions v SET end_user = s.end_user FROM sessions s
WHERE v.end_user IS NULL AND v.node_id = 'sbx_' || s.session_id;
-- 没有投递且归属已删除的版本只是等待缓存。停写期间丢弃，下次 Put 从 1 重建，不丢调用。
DELETE FROM inbox_versions v WHERE v.end_user IS NULL
AND NOT EXISTS (SELECT 1 FROM inbox i WHERE i.node_id = v.node_id);
ALTER TABLE inbox_versions ALTER COLUMN end_user SET NOT NULL;
