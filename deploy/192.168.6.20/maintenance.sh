#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
cd /www2/gowvp-v1
./check-storage.sh
exec 9>/run/lock/owl-v1-maintenance.lock
if [[ "${1:-monitor}" == backup ]]; then
  # 监控碰巧在运行时等待它结束，不能把跳过备份报告成成功。
  flock -w 180 9
else
  flock -n 9 || exit 0
fi

case "${1:-monitor}" in
  backup)
    task_day="$(date +%F)"
    task_week="$(date +%G-W%V)"
    install -d -m 700 backups/daily backups/weekly
    task_stage="$(mktemp -d /www2/gowvp-v1/backups/.stage.XXXXXX)"
    trap 'rm -rf -- "$task_stage"' EXIT
    cp -a .env docker-compose.yml "$task_stage/"
    install -d -m 700 "$task_stage/configs" "$task_stage/zlm-config"
    cp -a data/configs/config.toml data/configs/certs "$task_stage/configs/"
    if [[ -f data/configs/admin-credentials.toml ]]; then
      cp -a data/configs/admin-credentials.toml "$task_stage/configs/"
    fi
    if [[ -f data/configs/open-platform-keyring.json ]]; then
      cp -a data/configs/open-platform-keyring.json "$task_stage/configs/"
    fi
    cp -a data/zlm-config/. "$task_stage/zlm-config/"
    # pg_dump 为在线一致性快照；使用容器内同版本工具，不复制运行中的数据目录。
    docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml \
      exec -T postgres pg_dump -U postgres -d gowvp --format=custom --no-owner --no-acl \
      >"$task_stage/gowvp.dump"
    [[ -s "$task_stage/gowvp.dump" ]]
    docker compose --project-name owl-v1 --env-file .env --file docker-compose.yml \
      exec -T postgres pg_restore --list <"$task_stage/gowvp.dump" >/dev/null
    [[ -f postgres-init.sh ]] && cp -a postgres-init.sh "$task_stage/"
    tar -czf "backups/daily/$task_day.tar.gz.tmp" -C "$task_stage" .
    mv "backups/daily/$task_day.tar.gz.tmp" "backups/daily/$task_day.tar.gz"
    if [[ ! -e "backups/weekly/$task_week.tar.gz" ]]; then
      cp -a "backups/daily/$task_day.tar.gz" "backups/weekly/$task_week.tar.gz"
    fi
    find backups/daily -maxdepth 1 -type f -name '????-??-??.tar.gz' -mtime +6 -delete
    find backups/weekly -maxdepth 1 -type f -name '????-W??.tar.gz' -mtime +27 -delete
    printf 'Local consistent backup completed: %s\n' "$task_day"
    ;;
  monitor)
    for task_mount in /www /www2 /; do
      task_usage="$(df --output=pcent "$task_mount" | tail -1 | tr -dc '0-9')"
      if (( task_usage >= 80 )); then
        logger -p daemon.warning -t owl-v1 "Filesystem usage above 80%: $task_mount $task_usage%"
      fi
      printf 'Filesystem %s: %s%%\n' "$task_mount" "$task_usage"
    done
    # 只清理可重新生成的回放缓存，原录像由应用按数据库记录清理。
    if [[ -d /www/gowvp-data/recordings/.video-cache ]]; then
      find /www/gowvp-data/recordings/.video-cache -xdev -maxdepth 1 \
        -type f -name '*.mp4' -mmin +4320 -delete
    fi
    for task_spec in recordings:17592186044416 downloads:549755813888 events:107374182400 cover:107374182400 zlm-cache:107374182400; do
      task_name="${task_spec%%:*}"
      task_budget="${task_spec#*:}"
      task_bytes="$(timeout 120 ionice -c 3 nice -n 19 du -sx -B1 "/www/gowvp-data/$task_name" | cut -f1)"
      if (( task_bytes >= task_budget * 8 / 10 )); then
        logger -p daemon.warning -t owl-v1 "Directory budget above 80%: $task_name bytes=$task_bytes budget=$task_budget"
      fi
      printf 'Directory %s: %s bytes\n' "$task_name" "$task_bytes"
    done
    ;;
  *) printf 'Usage: %s backup|monitor\n' "$0" >&2; exit 1 ;;
esac
