#!/bin/sh
set -eu

config_path="${ZLM_CONFIG_PATH:-/opt/media/conf/config.ini}"
template_path="${ZLM_CONFIG_TEMPLATE:-/opt/media/default/config.ini}"

mkdir -p "$(dirname "$config_path")"
if [ ! -f "$config_path" ]; then
  cp "$template_path" "$config_path"
fi

api_debug="${ZLM_API_DEBUG:-0}"
case "$api_debug" in
  0|1) ;;
  *) echo "ZLM_API_DEBUG must be 0 or 1" >&2; exit 1 ;;
esac

python3 - "$config_path" "${OWL_MEDIA_SECRET:-}" "$api_debug" "${ZLM_HTTP_PORT:-}" <<'PY'
import pathlib
import re
import sys

path = pathlib.Path(sys.argv[1])
secret = sys.argv[2]
api_debug = sys.argv[3]
http_port = sys.argv[4]
if http_port and (not http_port.isdigit() or not 1 <= int(http_port) <= 65535):
    raise SystemExit("ZLM_HTTP_PORT must be between 1 and 65535")
if "\n" in secret or "\r" in secret:
    raise SystemExit("OWL_MEDIA_SECRET must not contain newlines")

lines = path.read_text(encoding="utf-8").replace("\r\n", "\n").split("\n")
section_pattern = re.compile(r"^\s*\[([^]]+)]\s*$")
prefixed_pattern = re.compile(r"^\s*api\.secret\s*=", re.IGNORECASE)
prefixed_api_debug_pattern = re.compile(r"^\s*api\.apiDebug\s*=", re.IGNORECASE)
secret_pattern = re.compile(r"^\s*secret\s*=", re.IGNORECASE)
api_debug_pattern = re.compile(r"^\s*apiDebug\s*=", re.IGNORECASE)
in_api = False
updated = False
prefixed_style = False
api_debug_updated = False
rewritten = []

for line in lines:
    if prefixed_pattern.match(line):
        prefixed_style = True
        if secret and not updated:
            rewritten.append(f"api.secret={secret}")
            updated = True
        elif not secret:
            rewritten.append(line)
        continue
    if prefixed_api_debug_pattern.match(line):
        prefixed_style = True
        if not api_debug_updated:
            rewritten.append(f"api.apiDebug={api_debug}")
            api_debug_updated = True
        continue
    section = section_pattern.match(line)
    if section:
        in_api = section.group(1).strip().lower() == "api"
        rewritten.append(line)
        continue
    if in_api and secret_pattern.match(line):
        if secret and not updated:
            rewritten.append(f"secret={secret}")
            updated = True
        elif not secret:
            rewritten.append(line)
        continue
    if in_api and api_debug_pattern.match(line):
        if not api_debug_updated:
            rewritten.append(f"apiDebug={api_debug}")
            api_debug_updated = True
        continue
    rewritten.append(line)

api_section = next(
    (
        index
        for index, line in enumerate(rewritten)
        if section_pattern.match(line)
        and section_pattern.match(line).group(1).strip().lower() == "api"
    ),
    None,
)

if secret and not updated:
    if api_section is None:
        if prefixed_style:
            rewritten.append(f"api.secret={secret}")
        else:
            while rewritten and rewritten[-1] == "":
                rewritten.pop()
            if rewritten:
                rewritten.append("")
            rewritten.extend(("[api]", f"secret={secret}"))
            api_section = len(rewritten) - 2
    else:
        rewritten.insert(api_section + 1, f"secret={secret}")

if not api_debug_updated:
    if api_section is None:
        if prefixed_style:
            rewritten.append(f"api.apiDebug={api_debug}")
        else:
            while rewritten and rewritten[-1] == "":
                rewritten.pop()
            if rewritten:
                rewritten.append("")
            rewritten.extend(("[api]", f"apiDebug={api_debug}"))
    else:
        insert_at = api_section + 1
        if insert_at < len(rewritten) and secret_pattern.match(rewritten[insert_at]):
            insert_at += 1
        rewritten.insert(insert_at, f"apiDebug={api_debug}")

if http_port:
    # 同时兼容 ZLM 保存的扁平配置与镜像模板的分段配置。
    http_section = None
    in_http = False
    port_updated = False
    for index, line in enumerate(rewritten):
        section = section_pattern.match(line)
        if section:
            in_http = section.group(1).strip().lower() == "http"
            if in_http:
                http_section = index
        flat_port = re.match(r"^\s*http\.port\s*=", line, re.IGNORECASE)
        if flat_port or (in_http and re.match(r"^\s*port\s*=", line, re.IGNORECASE)):
            rewritten[index] = ("http.port=" if flat_port else "port=") + http_port
            port_updated = True
    if not port_updated:
        if http_section is not None:
            rewritten.insert(http_section + 1, "port=" + http_port)
        elif prefixed_style:
            rewritten.append("http.port=" + http_port)
        else:
            rewritten.extend(("[http]", "port=" + http_port))

path.write_text("\n".join(rewritten) + "\n", encoding="utf-8")
PY

exec "$@"
