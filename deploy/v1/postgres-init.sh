#!/usr/bin/env bash
set -Eeuo pipefail

# 仅空数据目录首次初始化时执行。应用使用普通账号，管理员凭据不注入 Owl。
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
\getenv app_password OWL_POSTGRES_APP_PASSWORD
CREATE ROLE gowvp LOGIN PASSWORD :'app_password' NOSUPERUSER NOCREATEDB NOCREATEROLE;
GRANT CONNECT ON DATABASE gowvp TO gowvp;
GRANT USAGE, CREATE ON SCHEMA public TO gowvp;
SQL
