-- 所有账号数据按 (business_line,end_user) 归属；账号终态与注销记录永久保留，旧 ID 永不复用。
CREATE TABLE accounts (
 business_line text NOT NULL,
 end_user text NOT NULL,
 status text NOT NULL CHECK (status IN ('active','disabled','deleting','deleted')),
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,end_user)
);
-- 身份查找只保存加租户编码后的哈希；注销完成后删除绑定，同身份重新注册生成新 ID。
CREATE TABLE account_identities (
 identity_hash text PRIMARY KEY,
 business_line text NOT NULL,
 end_user text NOT NULL,
 FOREIGN KEY (business_line,end_user) REFERENCES accounts(business_line,end_user)
);
CREATE INDEX account_identities_owner ON account_identities(business_line,end_user);
CREATE TABLE account_devices (
 business_line text NOT NULL,
 end_user text NOT NULL,
 device_id text NOT NULL,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,end_user,device_id),
 FOREIGN KEY (business_line,end_user) REFERENCES accounts(business_line,end_user)
);
CREATE TABLE account_sessions (
 business_line text NOT NULL,
 end_user text NOT NULL,
 session_id text NOT NULL,
 access_hash text UNIQUE NOT NULL,
 refresh_hash text UNIQUE NOT NULL,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,end_user,session_id),
 FOREIGN KEY (business_line,end_user) REFERENCES accounts(business_line,end_user)
);
CREATE TABLE account_deletions (
 receipt_hash text UNIQUE,
 business_line text NOT NULL,
 end_user text NOT NULL,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,end_user),
 FOREIGN KEY (business_line,end_user) REFERENCES accounts(business_line,end_user)
);
