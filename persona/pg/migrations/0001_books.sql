-- owner 使用 bytea，保留 Go string 的全部字节（包括 NUL），避免分隔符编码歧义。
CREATE TABLE owners (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    PRIMARY KEY (business_line, end_user)
);

-- 仅编号及固定数量的 nil 集合标记是单例，不含实体列表或实体键目录。
CREATE TABLE book_meta (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

-- 实体键按领域身份编码；夹具允许重复身份时，再编码同身份内的出现次序。
-- 数组显式保存 ordinal；删除不压缩，插入用有限十进制中点，避免改写后续行。
-- 交谈、承诺仍是同事务物化，变更集是它们的事实源。
CREATE TABLE changesets (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    changeset_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, changeset_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX changesets_ordered ON changesets (business_line, end_user, ordinal);

CREATE TABLE exchanges (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    exchange_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, exchange_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX exchanges_ordered ON exchanges (business_line, end_user, ordinal);

CREATE TABLE commitments (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    commitment_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, commitment_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX commitments_ordered ON commitments (business_line, end_user, ordinal);

CREATE TABLE projects (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    project_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, project_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX projects_ordered ON projects (business_line, end_user, ordinal);

CREATE TABLE days (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    day_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, day_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX days_ordered ON days (business_line, end_user, ordinal);

CREATE TABLE weeks (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    week_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, week_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX weeks_ordered ON weeks (business_line, end_user, ordinal);

CREATE TABLE merges (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    merge_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, merge_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX merges_ordered ON merges (business_line, end_user, ordinal);

CREATE TABLE erasure_tombstones (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    erasure_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, erasure_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX erasure_tombstones_ordered ON erasure_tombstones (business_line, end_user, ordinal);

CREATE TABLE deletion_tasks (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    task_key bytea NOT NULL,
    ordinal numeric NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL,
    rebuild_pending boolean NOT NULL,
    PRIMARY KEY (business_line, end_user, task_key),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX deletion_tasks_ordered ON deletion_tasks (business_line, end_user, ordinal);

CREATE TABLE business_days (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    business_day bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, business_day),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

CREATE TABLE week_sessions (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    week_start bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, week_start),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

CREATE TABLE submitted (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    session_id bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, session_id),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

-- 新表建索引无需 CONCURRENTLY；未来对已有大表加索引用独立非事务迁移。
CREATE INDEX deletion_tasks_pending ON deletion_tasks (business_line, end_user)
    WHERE status = 'pending' OR rebuild_pending;

-- numeric(20,0) 覆盖 uint64；通过十进制字符串传参，不经过 int64/float64。
-- payload 为 NULL 表示整段已清空，主键保留引用编号与批次存在性。
-- live 独立于 JSONB 表示，含 U+0000 的载荷用可逆字符串封装后仍能走尚存片段索引。
CREATE TABLE capture_segments (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    business_day text NOT NULL,
    batch_number numeric(20,0) NOT NULL CHECK (batch_number BETWEEN 1 AND 18446744073709551615),
    segment_number bigint NOT NULL CHECK (segment_number >= 0),
    payload jsonb,
    live boolean NOT NULL CHECK (NOT live OR payload IS NOT NULL),
    PRIMARY KEY (business_line, end_user, business_day, batch_number, segment_number),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
CREATE INDEX capture_segments_live ON capture_segments
    (business_line, end_user, business_day, batch_number, segment_number)
    WHERE live;
