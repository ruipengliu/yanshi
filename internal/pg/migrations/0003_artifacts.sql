-- 工件元数据（docs/design/m2-artifacts.md）；字节内容在对象存储中（ADR-0011）。
CREATE TABLE artifacts (
    id         text        PRIMARY KEY,
    session_id text        NOT NULL,
    name       text        NOT NULL,
    mime_type  text        NOT NULL,
    size       bigint      NOT NULL,
    sha256     text        NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX artifacts_session ON artifacts (session_id, created_at);
