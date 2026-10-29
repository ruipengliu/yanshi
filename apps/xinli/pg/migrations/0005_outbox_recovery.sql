-- 只有 ID、位置与选择信封所需元数据；正文按内核当前权限读取。
CREATE TABLE outbox_recovery (
 business_line text PRIMARY KEY,
 version bigint NOT NULL,
 payload jsonb NOT NULL
);
CREATE TABLE outbox_sessions (
 business_line text NOT NULL,
 session_id text COLLATE "C" NOT NULL,
 end_user text NOT NULL,
 seen_round bigint NOT NULL DEFAULT 0,
 gone boolean NOT NULL DEFAULT false,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,session_id)
);
CREATE INDEX outbox_sessions_owner ON outbox_sessions(business_line,end_user);
CREATE TABLE outbox_receipts (
 business_line text NOT NULL,
 session_id text COLLATE "C" NOT NULL,
 seq numeric(20,0) NOT NULL,
 end_user text NOT NULL,
 envelope_id text,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,session_id,seq)
);
CREATE UNIQUE INDEX outbox_receipts_envelope ON outbox_receipts(business_line,envelope_id) WHERE envelope_id IS NOT NULL;
CREATE INDEX outbox_receipts_owner ON outbox_receipts(business_line,end_user);
