#!/usr/bin/env bash
set -Eeuo pipefail

# GoWVP 和 ZLM 已发布端口允许所有来源，仅更新本项目专用链。
# 原子替换旧白名单及 DROP，避免新增放行规则排在旧 DROP 后而失效。
iptables-restore -w --noflush <<'RULES'
*filter
:OWL-V1-ACCESS - [0:0]
-F OWL-V1-ACCESS
-A OWL-V1-ACCESS -j ACCEPT
COMMIT
RULES
iptables -w -C DOCKER-USER -o br-owl-v1 -j OWL-V1-ACCESS 2>/dev/null || \
  iptables -w -I DOCKER-USER 1 -o br-owl-v1 -j OWL-V1-ACCESS
