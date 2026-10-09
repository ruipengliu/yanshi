-- Memory 与 Grant（docs/design/m4-memory-grant.md，ADR-0016）。
-- pgvector 装在 public 中，供所有 schema 使用；私有化部署须预先安装该扩展。
CREATE EXTENSION IF NOT EXISTS vector SCHEMA public;

CREATE TABLE memories (
    id              text        PRIMARY KEY,
    business_line   text        NOT NULL,
    end_user        text        NOT NULL,
    category        text        NOT NULL,
    content         text        NOT NULL,
    source_session  text        NOT NULL,
    source_call     text        NOT NULL,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    -- 不固定维度：每个 EndUser 每个业务线至多数百条，检索时按所属方过滤后精确比较，无需向量索引。
    embedding       public.vector,
    embedding_model text
);
CREATE INDEX memories_owner ON memories (end_user, business_line, updated_at DESC, id);
CREATE INDEX memories_session ON memories (source_session);

CREATE TABLE memory_grants (
    id            text        PRIMARY KEY,
    end_user      text        NOT NULL,
    from_line     text        NOT NULL,
    to_line       text        NOT NULL,
    categories    text[]      NOT NULL,
    created_at    timestamptz NOT NULL,
    expires_at    timestamptz,
    revoked_at    timestamptz
);
CREATE INDEX memory_grants_reader ON memory_grants (end_user, to_line);
CREATE INDEX memory_grants_owner ON memory_grants (end_user, from_line);

-- 访问记录（不含内容）：随 Memory 删除。
CREATE TABLE memory_access (
    memory_id   text        NOT NULL REFERENCES memories (id) ON DELETE CASCADE,
    owner_line  text        NOT NULL,
    end_user    text        NOT NULL,
    reader_line text        NOT NULL,
    session_id  text        NOT NULL,
    run_id      text        NOT NULL,
    at          timestamptz NOT NULL
);
CREATE INDEX memory_access_owner ON memory_access (end_user, owner_line, at DESC);
CREATE INDEX memory_access_memory ON memory_access (memory_id);
