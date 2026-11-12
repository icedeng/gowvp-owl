#!/usr/bin/env python3
"""A/B/ZLM 受控发布。默认只读预检；--apply 排空、备份、升级，失败逆序回退程序。"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def require(ok, message):
    if not ok:
        raise ValueError(message)


def digest(path):
    h = hashlib.sha256()
    with open(path, 'rb') as f:
        for block in iter(lambda: f.read(1024 * 1024), b''):
            h.update(block)
    return h.hexdigest()


def safe_path(value):
    return isinstance(value, str) and re.fullmatch(r'/[A-Za-z0-9_.\-/]+', value) and '..' not in Path(value).parts and value != '/'


def validate(plan):
    require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,63}', plan['release_id']), '发布 ID 无效')
    parsed = urllib.parse.urlsplit(plan['api_base'])
    require(parsed.scheme in ('http', 'https') and parsed.hostname and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment, 'API 地址无效')
    require(set(plan['targets']) == {'a', 'b', 'zlm'}, '必须明确 A/B/ZLM 三个组件')
    require(plan.get('schema_backward_compatible') is True, '必须确认新增数据库结构向旧程序兼容；工具不自动回滚数据库')
    require(plan['steps'] and len(set(plan['steps'])) == len(plan['steps']) and set(plan['steps']) <= {'a', 'b', 'zlm'}, '发布顺序无效')
    identities = set()
    for role, t in plan['targets'].items():
        require(re.fullmatch(r'[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+', t['ssh']), 'SSH 目标无效')
        require(isinstance(t['port'], int) and 1 <= t['port'] <= 65535, 'SSH 端口无效')
        require(Path(t['key']).is_file(), 'SSH 密钥文件不存在')
        require(safe_path(t['directory']), '安装目录无效')
        require(0 < len(t['version']) <= 128, '缺少版本标签')
        for key in ('current_sha256', 'candidate_sha256'):
            require(re.fullmatch(r'[0-9a-f]{64}', t[key]), '组件摘要无效')
        if role == 'b':
            require(re.fullmatch(r'[A-Za-z0-9_-]+\.service', t['unit']), 'systemd unit 无效')
            require(re.fullmatch(r'[A-Za-z0-9_.\-/]+', t['binary_rel']) and '..' not in Path(t['binary_rel']).parts and not t['binary_rel'].startswith('/'), '二进制相对路径无效')
            require(safe_path(t['config_path']) and safe_path(t['state_path']), 'B 配置/状态路径无效')
            required_backups = {t['config_path'], t['state_path']}
            identity = (t['ssh'], t['port'], t['unit'])
        else:
            for key in ('service', 'project'):
                require(re.fullmatch(r'[A-Za-z0-9_-]+', t[key]), 'Compose 项目或服务无效')
            require(safe_path(t['compose_file']), 'Compose 文件必须为绝对安全路径')
            require(t.get('healthcheck_required') is True, 'Compose 服务须配置原生健康检查')
            required_backups = {t['compose_file']}
            identity = (t['ssh'], t['port'], t['project'], t['service'])
            if role == 'a':
                for key in ('database_service', 'database_user', 'database_name'):
                    require(re.fullmatch(r'[A-Za-z0-9_-]+', t[key]), 'PostgreSQL 备份参数无效')
        require(identity not in identities, '重复的组件部署目标')
        identities.add(identity)
        require(t.get('backup_paths') and required_backups <= set(t['backup_paths']), '备份必须包含部署配置及 B 状态目录')
        require(all(safe_path(v) for v in t['backup_paths']), '备份路径无效')
        if role in plan['steps']:
            require(re.fullmatch(r'[0-9a-f]{64}', t['file_sha256']) and digest(t['artifact']) == t['file_sha256'], '本地发布文件摘要不匹配')
            if role == 'b':
                with open(t['artifact'], 'rb') as f:
                    require(f.read(4) == b'\x7fELF', 'B 产物必须是 Linux ELF')
                require(t['candidate_sha256'] == t['file_sha256'], 'B 产物身份不一致')
    state = {k: v['current_sha256'] for k, v in plan['targets'].items()}
    require(state in plan['compatible_sets'], '当前组件组合不在兼容清单')
    for role in plan['steps']:
        state = {**state, role: plan['targets'][role]['candidate_sha256']}
        require(state in plan['compatible_sets'], f'{role} 升级后的中间组合不兼容')
    require(plan.get('gateways') and len(set(plan['gateways'])) == len(plan['gateways']), '网关列表缺失或重复')
    for gid in plan['gateways']:
        require(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_-]{0,63}', gid), '网关 ID 无效')


# 固定远端脚本，操作参数只通过 JSON 传递；不在管理 API 中开放远程命令。
REMOTE = r'''
import fcntl, hashlib, json, os, pathlib, shutil, subprocess, sys, tarfile, time
p=json.loads(sys.stdin.readline()); t=p['target']; role=p['role']; action=p['action']; release=p['release_id']; token=p['token']
base=pathlib.Path(t['directory']); assert base.is_dir(),'安装目录不存在'
backup=base/'backups'/release/role; lease=base/'.owl-release-active'; override=base/'compose.release.override.json'
def run(args): return subprocess.check_output(args,cwd=base,text=True,stderr=subprocess.PIPE,timeout=180).strip()
def atomic(path,data):
 path.parent.mkdir(parents=True,exist_ok=True)
 tmp=path.with_name(path.name+'.next')
 with open(tmp,'w') as f:
  os.chmod(tmp,0o600);f.write(data);f.flush();os.fsync(f.fileno())
 os.replace(tmp,path)
 fd=os.open(path.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
def compose(*args):
 files=['-f',t['compose_file']]
 if override.exists(): files+=['-f',str(override)]
 return run(['docker','compose','-p',t['project'],*files,*args])
def container():
 cid=compose('ps','-q',t['service']); assert cid and '\n' not in cid,'目标容器不唯一或未运行';return cid
def image():return run(['docker','inspect','-f','{{.Image}}',container()]).removeprefix('sha256:')
def sha(path):
 h=hashlib.sha256()
 with open(path,'rb') as f:
  for data in iter(lambda:f.read(1024*1024),b''):h.update(data)
 return h.hexdigest()
def identity():
 if role=='b':
  binary=base/'current'/t['binary_rel']
  assert (base/'current').is_symlink(),'B 必须采用 current 软链布局'
  assert run(['systemctl','is-active',t['unit']])=='active','B 服务未运行'
  pid=run(['systemctl','show','-p','MainPID','--value',t['unit']])
  assert int(pid)>0 and sha('/proc/'+pid+'/exe')==sha(binary),'B 运行进程与 current 不一致'
  return sha(binary)
 health=run(['docker','inspect','-f','{{if .State.Health}}{{.State.Health.Status}}{{end}}',container()])
 assert health=='healthy','容器未通过原生健康检查'
 return image()
if action=='preflight':
 assert not lease.exists(),'已有发布租约，须先检查原发布结果'
 assert not backup.exists(),'发布 ID 已用，禁止覆盖备份'
 for path in t['backup_paths']:assert pathlib.Path(path).exists(),'备份源不存在'
 current=identity();assert current==t['current_sha256'],'线上组件身份与计划不一致'
 if role=='b':
  unit=run(['systemctl','show','-p','ExecStart','--value',t['unit']])
  assert str(base/'current'/t['binary_rel']) in unit and t['config_path'] in unit,'B unit 与清单路径不符'
 print(json.dumps({'current_sha256':current}));sys.exit()
# 跨 SSH 调用持有显式租约；中途 CLI 退出时不会自动让其他发布穿插。
lock=open(base/'.gateway-release.lock','a');fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
if action=='acquire':
 assert identity()==t['current_sha256'],'取得租约前组件发生变化'
 try:
  lease.mkdir(mode=0o700);atomic(lease/'owner.json',json.dumps({'token':token,'release_id':release,'created_at':time.time()}))
 except FileExistsError:
  assert json.loads((lease/'owner.json').read_text())['token']==token,'发布租约冲突'
 print(json.dumps({'locked':True}));sys.exit()
if action=='release' and not lease.exists():print('{}');sys.exit()
assert json.loads((lease/'owner.json').read_text())['token']==token,'发布租约已失效'
if action=='release':
 (lease/'owner.json').unlink();lease.rmdir();print('{}');sys.exit()
if action=='stage':
 stage=base/'packages'/release/role;stage.mkdir(parents=True,mode=0o700);os.chmod(stage,0o700)
 print(json.dumps({'stage':str(stage/'artifact')}));sys.exit()
if action=='upgrade':
 assert not backup.exists(),'此步骤已开始，禁止重复升级'
 stage=base/'packages'/release/role/'artifact';assert sha(stage)==t['file_sha256'],'远端产物摘要不一致'
 old=str((base/'current').resolve()) if role=='b' else 'sha256:'+image()
 backup.mkdir(parents=True,mode=0o700);os.chmod(backup,0o700)
 prior=json.loads(override.read_text()) if override.exists() else {'services':{}}
 previous={'previous':old,'override_entry':prior.get('services',{}).get(t.get('service',''))}
 atomic(backup/'previous.json',json.dumps(previous)) # 停服前保存，失败可恢复原程序。
 if role=='b':run(['systemctl','stop',t['unit']])
 else:compose('stop',t['service'])
 with tarfile.open(backup/'configuration.tar','w') as archive:
  os.chmod(backup/'configuration.tar',0o600)
  for path in t['backup_paths']:archive.add(path,arcname=path.lstrip('/'),recursive=True)
 if role=='a':
  with open(backup/'database.dump','wb') as out:
   os.chmod(backup/'database.dump',0o600)
   subprocess.run(['docker','compose','-p',t['project'],'-f',t['compose_file'],'exec','-T',t['database_service'],'pg_dump','-U',t['database_user'],'-d',t['database_name'],'-Fc'],cwd=base,stdout=out,stderr=subprocess.PIPE,check=True,timeout=180)
 if role=='b':
  dest=base/'releases'/release/t['binary_rel'];dest.parent.mkdir(parents=True,exist_ok=False)
  shutil.copyfile(stage,dest);os.chmod(dest,0o755)
  link=base/'current.next'
  if link.is_symlink():link.unlink()
  link.symlink_to(base/'releases'/release);os.replace(link,base/'current');run(['systemctl','start',t['unit']])
 else:
  run(['docker','load','-i',str(stage)])
  candidate='sha256:'+t['candidate_sha256'];run(['docker','image','inspect',candidate])
  prior.setdefault('services',{})[t['service']]={'image':candidate}
  atomic(override,json.dumps(prior));compose('up','-d','--no-deps',t['service'])
 print(json.dumps({'previous':old,'backup':str(backup)}));sys.exit()
if action=='verify':
 current=identity();expected=p['expected_sha256'];assert current==expected,'发布后组件摘要不一致'
 print(json.dumps({'verified_sha256':current}));sys.exit()
if action=='rollback':
 if not (backup/'previous.json').exists():print(json.dumps({'previous':'unchanged'}));sys.exit()
 previous=json.loads((backup/'previous.json').read_text());old=previous['previous']
 if role=='b':
  run(['systemctl','stop',t['unit']]);link=base/'current.next'
  if link.is_symlink():link.unlink()
  link.symlink_to(old);os.replace(link,base/'current');run(['systemctl','start',t['unit']])
 else:
  value=json.loads(override.read_text()) if override.exists() else {'services':{}}
  entry=previous['override_entry']
  entry=dict(entry or {});entry['image']=old
  value.setdefault('services',{})[t['service']]=entry
  atomic(override,json.dumps(value));compose('up','-d','--no-deps',t['service'])
 # 不自动恢复数据库/B 状态，避免丢弃发布期间接纳的报警。
 print(json.dumps({'previous':old}));sys.exit()
raise ValueError('unsupported action')
'''


class Release:
    def __init__(self, plan):
        self.plan = plan
        self.base = plan['api_base'].rstrip('/')
        self.token = os.environ['OWL_RELEASE_TOKEN']
        self.lease_token = str(uuid.uuid4())
        self.evidence = []

    def api(self, method, path, body=None, revision=None):
        headers = {'Authorization': 'Bearer ' + self.token, 'Content-Type': 'application/json', 'Idempotency-Key': str(uuid.uuid4())}
        if revision is not None:
            headers['If-Match'] = '"' + str(revision) + '"'
        req = urllib.request.Request(self.base + path, data=json.dumps(body).encode() if body is not None else None, method=method, headers=headers)
        with urllib.request.urlopen(req, timeout=20) as response:
            return json.load(response)

    def remote(self, role, action, expected=None):
        t = self.plan['targets'][role]
        payload = {'target': t, 'role': role, 'action': action, 'release_id': self.plan['release_id'], 'token': self.lease_token, 'expected_sha256': expected}
        code = 'import io,sys\nsys.stdin=io.StringIO(' + repr(json.dumps(payload) + '\n') + ')\n' + REMOTE
        result = subprocess.run(['ssh', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=10', '-p', str(t['port']), '-i', t['key'], t['ssh'], 'python3 -'], input=code, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=600)
        require(result.returncode == 0, f'{role} {action} 失败；原始错误未写入报告以免暴露配置')
        value = json.loads(result.stdout)
        self.evidence.append({'component': role, 'action': action, **value})
        return value

    def gateway(self, gid):
        return self.api('GET', '/access_gateways/' + gid)

    def state(self, gid, state):
        g = self.gateway(gid)
        if g['admin_state'] == state and not g.get('pending_operation_id'):
            return
        op = self.api('PUT', '/access_gateways/' + gid + '/state', {'state': state, 'force': False, 'drain_timeout_ms': 60000}, g['revision'])
        if 'operation_id' in op:
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                result = self.api('GET', '/access_gateways/' + gid + '/operations/' + op['operation_id'])
                if result['status'] == 'succeeded':
                    return
                require(result['status'] not in ('failed', 'cancelled', 'waiting_external'), '排空/恢复未完成；保留维护状态，请处理活动会话或待执行操作')
                time.sleep(1)
            raise TimeoutError('排空/恢复超时；未强制中断会话，请检查待执行操作')

    def verify(self, components):
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                for role, sha in components.items():
                    self.remote(role, 'verify', sha)
                healthy = all((g := self.gateway(gid))['connection_state'] == 'connected' and g['health_state'] == 'ready' and g['applied_revision'] == g['config_revision'] for gid in self.plan['gateways'])
                if healthy:
                    return
            except (urllib.error.URLError, TimeoutError, ValueError):
                pass # 重启期间的暂时离线持续观察，不能第一次 502 就判定升级失败。
            time.sleep(2)
        raise TimeoutError('组件健康检查或网关恢复未通过')

    def report(self, role, outcome, previous):
        t = self.plan['targets'][role]
        body = {'release_id': self.plan['release_id'], 'component': role, 'version': t['version'], 'sha256': t['candidate_sha256'], 'outcome': outcome, 'previous': previous}
        for gid in self.plan['gateways']:
            try:
                g = self.gateway(gid)
                self.api('POST', '/access_gateways/' + gid + '/release-reports', body, g['revision'])
            except Exception:
                self.evidence.append({'gateway': gid, 'pending_report': body}) # 回滚旧 A 无报告接口时保留本地待补报。

    def apply(self):
        acquired, drained, started, previous = [], [], [], {}
        expected = {role: t['current_sha256'] for role, t in self.plan['targets'].items()}
        safe_to_unlock = True
        try:
            for role in ('a', 'b', 'zlm'):
                self.remote(role, 'acquire'); acquired.append(role)
            for gid in self.plan['gateways']:
                g = self.gateway(gid)
                require(g['admin_state'] == 'enabled' and not g.get('pending_operation_id'), '网关有待执行操作或未启用')
                drained.append(gid) # API 响应丢失时也检查/恢复维护状态。
                self.state(gid, 'draining')
            for role in self.plan['steps']:
                t = self.plan['targets'][role]
                stage = self.remote(role, 'stage')['stage']
                subprocess.run(['scp', '-q', '-o', 'BatchMode=yes', '-o', 'StrictHostKeyChecking=yes', '-P', str(t['port']), '-i', t['key'], t['artifact'], t['ssh'] + ':' + stage], check=True, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=600)
                started.append(role)
                previous[role] = self.remote(role, 'upgrade')['previous']
                expected[role] = t['candidate_sha256']
                self.verify(expected)
            for gid in drained:
                self.state(gid, 'enabled')
            self.verify(expected)
        except Exception:
            rollback_ok = True
            for role in reversed(started):
                try:
                    previous[role] = self.remote(role, 'rollback')['previous']
                    expected[role] = self.plan['targets'][role]['current_sha256']
                except Exception:
                    rollback_ok = False
            try:
                if rollback_ok:
                    for gid in drained:
                        self.state(gid, 'enabled')
                    if started or drained:
                        self.verify(expected)
            except Exception:
                rollback_ok = False
            for role in started:
                self.report(role, 'rolled_back' if rollback_ok else 'rollback_failed', previous.get(role, 'unknown'))
            safe_to_unlock = rollback_ok
            self.evidence.append({'outcome': 'rolled_back' if rollback_ok else 'manual_recovery_required'})
            raise
        else:
            for role in started:
                self.report(role, 'succeeded', previous[role])
            self.evidence.append({'outcome': 'succeeded'})
        finally:
            if safe_to_unlock:
                for role in reversed(acquired):
                    try:
                        self.remote(role, 'release')
                    except Exception:
                        self.evidence.append({'component': role, 'release_lease': 'manual_check_required'})
            else:
                self.evidence.append({'release_id': self.plan['release_id'], 'lease_retained': True, 'lease_token': self.lease_token})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('manifest')
    parser.add_argument('--apply', action='store_true')
    parser.add_argument('--report', default='gateway-release-result.json')
    args = parser.parse_args()
    plan = json.loads(Path(args.manifest).read_text())
    validate(plan)
    # 先预留私有报告文件，防止所有远端操作完成后才发现本地无法写报告。
    fd = os.open(args.report, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    release = None
    try:
        release = Release(plan)
        for role in ('a', 'b', 'zlm'):
            release.remote(role, 'preflight')
        for gid in plan['gateways']:
            g = release.gateway(gid)
            require(g['admin_state'] == 'enabled' and not g.get('pending_operation_id'), '网关未启用或有待执行操作')
        if args.apply:
            release.apply()
        print('发布完成' if args.apply else '只读预检完成；使用 --apply 执行此清单')
    finally:
        with os.fdopen(fd, 'w') as f:
            json.dump(release.evidence if release else [], f, ensure_ascii=False, indent=2)


if __name__ == '__main__':
    try:
        main()
    except Exception:
        print('发布未完成，请查看私有报告中的阶段和恢复状态；未输出可能含凭据的异常正文。', file=sys.stderr)
        sys.exit(1)
