-- 仅缓存定时的最早到期，日志仍是事实源。
ALTER TABLE sessions ADD COLUMN next_due timestamptz;
