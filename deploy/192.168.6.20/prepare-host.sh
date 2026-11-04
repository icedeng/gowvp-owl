#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

[[ $EUID -eq 0 ]]
[[ "$(findmnt -n -o SOURCE --target /www)" == /dev/sdb2 ]]
[[ "$(findmnt -n -o SOURCE --target /www2)" == /dev/sda4 ]]
mountpoint -q /www
mountpoint -q /www2

# 只允许在容器运行时为空时迁移，旧目录和配置保留用于回退。
[[ -z "$(docker ps -aq)" ]]
[[ -z "$(docker image ls -q)" ]]
[[ "$(ctr namespaces list -q)" == moby ]]
[[ -z "$(ctr -n moby containers list -q)" ]]
[[ -z "$(ctr -n moby images list -q)" ]]
[[ ! -e /www2/containerd ]]
[[ ! -e /etc/systemd/system/docker.service.d/40-owl-storage.conf ]]
[[ ! -e /etc/systemd/system/containerd.service.d/40-owl-storage.conf ]]

install -d -m 700 /www2/gowvp-v1/backups/host-before-deploy
cp -a /etc/containerd/config.toml /www2/gowvp-v1/backups/host-before-deploy/containerd-config.toml

systemctl stop docker.service docker.socket
systemctl stop containerd.service

restore_runtime() {
  cp -a /www2/gowvp-v1/backups/host-before-deploy/containerd-config.toml /etc/containerd/config.toml
  systemctl daemon-reload
  systemctl start containerd.service docker.socket docker.service
}
trap restore_runtime ERR

install -d -m 700 /www2/containerd
rsync -aHAX /var/lib/containerd/ /www2/containerd/
[[ "$(grep -c '^#root = "/var/lib/containerd"' /etc/containerd/config.toml)" == 1 ]]
sed -i 's|^#root = "/var/lib/containerd"|root = "/www2/containerd"|' /etc/containerd/config.toml

install -d /etc/systemd/system/docker.service.d /etc/systemd/system/containerd.service.d
printf '[Unit]\nRequiresMountsFor=/www /www2\n' > /etc/systemd/system/docker.service.d/40-owl-storage.conf
printf '[Unit]\nRequiresMountsFor=/www2/containerd\n' > /etc/systemd/system/containerd.service.d/40-owl-storage.conf
systemctl daemon-reload
systemctl start containerd.service docker.socket docker.service
systemctl is-active --quiet containerd.service docker.service
containerd config dump | grep -F "root = '/www2/containerd'"
trap - ERR

install -d -m 700 \
  /www2/gowvp-v1/packages \
  /www2/gowvp-v1/data/configs/certs \
  /www2/gowvp-v1/data/logs \
  /www2/gowvp-v1/data/postgres \
  /www2/gowvp-v1/data/zlm-config \
  /www2/gowvp-v1/data/zlm-fflogs \
  /www/gowvp-data/recordings \
  /www/gowvp-data/downloads \
  /www/gowvp-data/events \
  /www/gowvp-data/cover \
  /www/gowvp-data/zlm-cache
printf '%s\n' 'Host storage prepared; original containerd data retained at /var/lib/containerd'
