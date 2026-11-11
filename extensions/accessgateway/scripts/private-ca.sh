#!/usr/bin/env bash
set -euo pipefail
# 用法：private-ca.sh 输出目录 网关证书SAN 平台ID
# SAN 示例：DNS:gateway.example.com,IP:203.0.113.10
# CA 私钥必须加密；设置 CA_KEY_PASSWORD_FILE 时从该文件读取，否则由 OpenSSL 提示输入。
[[ $# == 3 ]] || { printf '%s\n' '用法：private-ca.sh 输出目录 DNS:gateway.example.com,IP:203.0.113.10 owl-a' >&2; exit 1; }
cert_dir=$1
server_san=$2
platform_id=$3
[[ "$platform_id" =~ ^[A-Za-z0-9._-]{1,64}$ ]] || { printf '%s\n' '平台 ID 格式无效' >&2; exit 1; }
[[ "$server_san" =~ ^(DNS:|IP:)[A-Za-z0-9:.,_-]+$ ]] || { printf '%s\n' '证书 SAN 格式无效' >&2; exit 1; }
umask 077
mkdir -p "$cert_dir"
chmod 0700 "$cert_dir"
for name in ca.key ca.crt server.key server.crt client.key client.crt; do
  [[ ! -e "$cert_dir/$name" ]] || { printf '目标已存在，拒绝覆盖：%s\n' "$cert_dir/$name" >&2; exit 1; }
done
pass_in=()
pass_out=()
if [[ -n ${CA_KEY_PASSWORD_FILE:-} ]]; then
  [[ -f "$CA_KEY_PASSWORD_FILE" ]] || { printf '%s\n' 'CA 密码文件不存在' >&2; exit 1; }
  pass_in=(-passin "file:$CA_KEY_PASSWORD_FILE")
  pass_out=(-pass "file:$CA_KEY_PASSWORD_FILE")
fi
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -aes-256-cbc "${pass_out[@]}" -out "$cert_dir/ca.key"
openssl req -new -x509 -sha256 -days 3650 -key "$cert_dir/ca.key" "${pass_in[@]}" -out "$cert_dir/ca.crt" \
  -subj '/CN=Owl Access Gateway Private CA' -addext 'basicConstraints=critical,CA:TRUE,pathlen:0' -addext 'keyUsage=critical,keyCertSign,cRLSign'
for identity in server client; do
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$cert_dir/$identity.key"
  cn=$platform_id
  if [[ "$identity" == server ]]; then cn=owl-gateway; fi
  openssl req -new -sha256 -key "$cert_dir/$identity.key" -out "$cert_dir/$identity.csr" -subj "/CN=$cn"
  {
    printf '%s\n' 'basicConstraints=critical,CA:FALSE' 'keyUsage=critical,digitalSignature,keyEncipherment'
    if [[ "$identity" == server ]]; then
      printf 'extendedKeyUsage=serverAuth\nsubjectAltName=%s\n' "$server_san"
    else
      printf '%s\n' 'extendedKeyUsage=clientAuth'
    fi
  } > "$cert_dir/$identity.ext"
  openssl x509 -req -sha256 -days 365 -in "$cert_dir/$identity.csr" -CA "$cert_dir/ca.crt" -CAkey "$cert_dir/ca.key" \
    "${pass_in[@]}" -CAcreateserial -out "$cert_dir/$identity.crt" -extfile "$cert_dir/$identity.ext"
  rm "$cert_dir/$identity.csr" "$cert_dir/$identity.ext"
done
openssl verify -CAfile "$cert_dir/ca.crt" -purpose sslserver "$cert_dir/server.crt"
openssl verify -CAfile "$cert_dir/ca.crt" -purpose sslclient "$cert_dir/client.crt"
printf '%s\n' '证书已生成。CA 私钥留在签发机；B 仅挂载 ca.crt/server.crt/server.key；A 导入 ca.crt/client.crt/client.key。'
