-- 每个分片库各一行；实例 ID 随备库复制，不以机器或连接地址作为身份。
CREATE TABLE IF NOT EXISTS public.shard_identity (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    instance_id uuid NOT NULL,
    shard_id integer NOT NULL CHECK (shard_id >= 0)
);
