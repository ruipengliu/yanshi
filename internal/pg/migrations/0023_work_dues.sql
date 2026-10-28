-- 远期停放不参与认领扫描；ord 保留 Park 后的同优先级顺序。
CREATE TABLE work_dues (
    session_id    text PRIMARY KEY,
    business_line text NOT NULL,
    priority      smallint NOT NULL,
    ord           bigint NOT NULL,
    due_at        timestamptz NOT NULL
);
