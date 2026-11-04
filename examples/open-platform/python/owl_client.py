"""Owl 签名及回调验签示例，仅依赖 Python 标准库。密钥放服务端环境变量。"""
import base64
import hashlib
import hmac
import json
import os
import re
import secrets
import sys
import time
from urllib.parse import quote, unquote_to_bytes
from urllib.request import Request, urlopen


def canonical_query(raw):
    pairs, seen = [], set()
    for pair in raw.split('&') if raw else []:
        key, _, value = pair.partition('=')
        if re.search(r'%(?![0-9a-fA-F]{2})', key + value):
            raise ValueError('非法百分号编码')
        key = unquote_to_bytes(key).decode('utf-8', errors='strict')
        value = unquote_to_bytes(value).decode('utf-8', errors='strict')
        if not key or key in seen:
            raise ValueError('空参数名或重复参数')
        seen.add(key)
        pairs.append((quote(key, safe='-._~'), quote(value, safe='-._~')))
    return '&'.join(f'{key}={value}' for key, value in sorted(pairs))


def decoded_key(sk):
    raw = base64.b64decode(sk + '=' * (-len(sk) % 4), altchars=b'-_', validate=True)
    if len(raw) != 32:
        raise ValueError('SK 必须为 32 字节')
    return raw


def canonical(audience, method, path, query, content_type, timestamp, nonce, idempotency, body):
    if not re.fullmatch(r'/[A-Za-z0-9/._~-]+', path) or any(x in ('', '.', '..') for x in path.split('/')[1:]):
        raise ValueError('非规范路径')
    return '\n'.join(['OWL-HMAC-SHA256', audience, method.upper(), path, canonical_query(query), content_type,
                      str(timestamp), nonce, idempotency, hashlib.sha256(body).hexdigest()])


def signed_headers(ak, sk, audience, method, path, query='', body=b'', idempotency=''):
    timestamp = str(int(time.time()))
    nonce = secrets.token_urlsafe(24)
    content_type = '' if method.upper() in ('GET', 'DELETE') else 'application/json'
    text = canonical(audience, method, path, query, content_type, timestamp, nonce, idempotency, body)
    signature = hmac.new(decoded_key(sk), text.encode(), hashlib.sha256).hexdigest()
    headers = {'Authorization': f'OWL-HMAC-SHA256 Credential={ak},Signature={signature}',
               'X-Owl-Audience': audience, 'X-Owl-Timestamp': timestamp, 'X-Owl-Nonce': nonce,
               'X-Owl-Content-SHA256': hashlib.sha256(body).hexdigest()}
    if content_type:
        headers['Content-Type'] = content_type
    if idempotency:
        headers['Idempotency-Key'] = idempotency
    return headers


def verify_callback(headers, raw_body, callback_sk):
    """验签后仍须持久化防重放，校验 payload ID，按 delivery_id 幂等入库后返回2xx。"""
    try:
        h = {key.lower(): value for key, value in headers.items()}
        ts, nonce = h['x-owl-timestamp'], h['x-owl-nonce']
        if str(int(ts)) != ts or abs(time.time() - int(ts)) > 300 or len(base64.urlsafe_b64decode(nonce + '=' * (-len(nonce) % 4))) < 16:
            return False
        digest = hashlib.sha256(raw_body).hexdigest()
        if digest != h['x-owl-content-sha256']:
            return False
        text = '\n'.join(['OWL-CALLBACK-HMAC-SHA256', ts, nonce, h['x-owl-event-id'], h['x-owl-delivery-id'], digest])
        expected = 'OWL-CALLBACK-HMAC-SHA256 Signature=' + hmac.new(decoded_key(callback_sk), text.encode(), hashlib.sha256).hexdigest()
        return hmac.compare_digest(expected, h['authorization'])
    except (KeyError, ValueError):
        return False


def vector():
    body = b'{"channel_id":"c1"}'
    sk = base64.urlsafe_b64encode(bytes(range(32))).decode().rstrip('=')
    text = canonical('owl-test', 'POST', '/openapi/v1/snapshot-tasks', 'q=A+B%20C&empty=&label=%E5%91%8A%E8%AD%A6',
                     'application/json', '1700000000', 'AAECAwQFBgcICQoLDA0ODw', 'capture-001', body)
    return text, hmac.new(decoded_key(sk), text.encode(), hashlib.sha256).hexdigest()


if __name__ == '__main__':
    if '--vector' in sys.argv:
        print(vector()[1])
    else:
        base = os.environ['OWL_API_BASE'].rstrip('/')
        if not (base.startswith('https://') or (os.environ.get('OWL_ALLOW_HTTP') == 'true' and base.startswith('http://'))):
            raise ValueError('HTTP 部署请显式设置 OWL_ALLOW_HTTP=true')
        path = '/openapi/v1/devices'
        headers = signed_headers(os.environ['OWL_AK'], os.environ['OWL_SK'], os.environ['OWL_AUDIENCE'], 'GET', path)
        with urlopen(Request(base + path, headers=headers), timeout=15) as response:
            print(json.dumps(json.load(response), ensure_ascii=False, indent=2))
