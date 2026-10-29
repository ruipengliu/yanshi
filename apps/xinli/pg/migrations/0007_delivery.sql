-- 先接收再处理；正文不进入应用通知存储。
ALTER TABLE outbox_receipts ADD COLUMN processed boolean NOT NULL DEFAULT false;
CREATE TABLE delivery_state (
 business_line text NOT NULL,
 end_user text NOT NULL,
 payload jsonb NOT NULL,
 PRIMARY KEY (business_line,end_user)
);
