-- 时区、自然到期状态与原文版本留在业务日 payload；扫描列与 payload 同事务更新。
-- NULL 表示尚未装载升级的旧业务日，扫描也发现它们，避免无人访问的旧原文漏清。
ALTER TABLE business_days ADD COLUMN expires_at timestamptz;
-- 空业务日也没有待清理原文；领域的 Expired 状态单独保存在 payload。
ALTER TABLE business_days ADD COLUMN capture_cleanup_done boolean NOT NULL DEFAULT false;
