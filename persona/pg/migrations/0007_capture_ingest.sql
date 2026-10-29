-- 暂存正文属于原文区，与业务日、世代及 owner 锁同事务提交，不进入账本快照。
CREATE TABLE capture_pending (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    business_day text NOT NULL,
    session_id bytea NOT NULL,
    seq numeric(20,0) NOT NULL CHECK (seq BETWEEN 1 AND 18446744073709551614),
    generation numeric(20,0) NOT NULL CHECK (generation BETWEEN 0 AND 18446744073709551615),
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, business_day, session_id, seq, generation),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);

-- 只含顺序、摘要哈希与引用；正文在正式追加后即从暂存区移走。
CREATE TABLE capture_streams (
    business_line bytea NOT NULL,
    end_user bytea NOT NULL,
    stream_id bytea NOT NULL,
    payload jsonb NOT NULL,
    PRIMARY KEY (business_line, end_user, stream_id),
    FOREIGN KEY (business_line, end_user) REFERENCES owners ON DELETE CASCADE
);
