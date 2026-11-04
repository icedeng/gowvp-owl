#!/usr/bin/env bash
set -Eeuo pipefail

# 同名目录存在不代表数据盘已挂载，启动前同时核对设备和文件系统。
[[ "$(findmnt -n -o SOURCE --target /www)" == /dev/sdb2 ]]
[[ "$(findmnt -n -o FSTYPE --target /www)" == xfs ]]
[[ "$(findmnt -n -o SOURCE --target /www2)" == /dev/sda4 ]]
[[ "$(findmnt -n -o FSTYPE --target /www2)" == ext4 ]]
mountpoint -q /www
mountpoint -q /www2

for task_dir in \
  /www2/gowvp-v1/data/configs \
  /www2/gowvp-v1/data/logs \
  /www2/gowvp-v1/data/postgres \
  /www2/gowvp-v1/data/zlm-config \
  /www2/gowvp-v1/data/zlm-fflogs \
  /www/gowvp-data/recordings \
  /www/gowvp-data/downloads \
  /www/gowvp-data/events \
  /www/gowvp-data/cover \
  /www/gowvp-data/zlm-cache; do
  [[ -d "$task_dir" && ! -L "$task_dir" ]]
done

[[ -f /www2/gowvp-v1/data/configs/config.toml ]]
printf '%s\n' 'GoWVP storage mounts verified'
