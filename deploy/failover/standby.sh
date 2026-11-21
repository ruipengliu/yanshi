#!/usr/bin/env bash
set -euo pipefail
if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
  mkdir -p "$PGDATA"
  chown postgres:postgres "$PGDATA"
  chmod 700 "$PGDATA"
  export PGPASSWORD="$POSTGRES_PASSWORD"
  # STANDBY_NAME 是同步复制登记的名字：主备演练为 failover_standby，整片搬迁的新主机为 shard_move_target。
  gosu postgres pg_basebackup -d "host=primary user=yanshi application_name=${STANDBY_NAME:-failover_standby}" -D "$PGDATA" -R -X stream --checkpoint=fast
fi
# 提升后没有第二备库；保持本地持久提交，不冒充仍有同步冗余。
exec gosu postgres postgres -c synchronous_standby_names='' -c synchronous_commit=on
