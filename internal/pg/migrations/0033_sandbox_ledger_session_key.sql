-- 旧账本必须有真实 Session 才能保留重试身份；NULL 历史行需先核实归属，不能猜测或清空。
ALTER TABLE sandbox_ledger ALTER COLUMN session_id SET NOT NULL;
ALTER TABLE sandbox_ledger DROP CONSTRAINT sandbox_ledger_pkey;
ALTER TABLE sandbox_ledger ADD CONSTRAINT sandbox_ledger_pkey PRIMARY KEY USING INDEX sandbox_ledger_session_call;
