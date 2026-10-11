-- 恢复名额（ADR-0029 修订，延展性评审 §8.2）：后台池中，用户刚做出审批或回答的长任务先被认领。
-- 优先级改为 Background 0、Resumed 1、Interactive 2：原来的交互优先级 1 改为 2，新增项的默认值随之改变。
UPDATE work_items SET priority = 2 WHERE priority = 1;
UPDATE sandbox_items SET priority = 2 WHERE priority = 1;
UPDATE janitor_items SET priority = 2 WHERE priority = 1;
ALTER TABLE work_items ALTER COLUMN priority SET DEFAULT 2;
ALTER TABLE sandbox_items ALTER COLUMN priority SET DEFAULT 2;
ALTER TABLE janitor_items ALTER COLUMN priority SET DEFAULT 2;
