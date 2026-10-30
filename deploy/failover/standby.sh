#!/usr/bin/env bash
set -euo pipefail
if [[ ! -s "$PGDATA/PG_VERSION" ]]; then
  mkdir -p "$PGDATA"
  chown postgres:postgres "$PGDATA"
  chmod 700 "$PGDATA"
  export PGPASSWORD="$POSTGRES_PASSWORD"
  gosu postgres pg_basebackup -d 'host=primary user=yanshi application_name=failover_standby' -D "$PGDATA" -R -X stream --checkpoint=fast
fi
# 提升后没有第二备库；保持本地持久提交，不冒充仍有同步冗余。
exec gosu postgres postgres -c synchronous_standby_names='' -c synchronous_commit=on
