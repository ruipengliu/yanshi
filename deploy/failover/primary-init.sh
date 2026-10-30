#!/usr/bin/env bash
set -euo pipefail
# 初始化事务结束后才启用同步等待，否则尚未启动的备库会阻塞 entrypoint。
printf '\nhost replication yanshi all scram-sha-256\n' >> "$PGDATA/pg_hba.conf"
printf "\nsynchronous_standby_names = 'FIRST 1 (failover_standby)'\n" >> "$PGDATA/postgresql.conf"
