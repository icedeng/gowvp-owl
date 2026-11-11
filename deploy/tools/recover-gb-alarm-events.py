#!/usr/bin/env python3
"""受控恢复旧报警到事件中心；默认预览，--apply 前备份，不重放实时通知或协议副作用。"""
import argparse
import datetime as dt
import hashlib
import json
import os
import pathlib
import re
import subprocess


def literal(value):
    return "'" + str(value).replace("'", "''") + "'"


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('device-id', 'gb-device-id', 'channel-id', 'gb-channel-id', 'start', 'end', 'backup-dir'):
        parser.add_argument('--' + name, required=True)
    parser.add_argument('--container', default='owl-v1-postgres-1')
    parser.add_argument('--database', default='gowvp')
    parser.add_argument('--apply', action='store_true')
    args = parser.parse_args()
    start, end = (dt.datetime.fromisoformat(value) for value in (args.start, args.end))
    if not start.tzinfo or not end.tzinfo or end <= start:
        raise ValueError('必须提供有时区的有限时间窗口')
    def sql(query):
        return subprocess.run(['docker', 'exec', '-i', args.container, 'psql', '-X', '-q', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', args.database, '-At'], input=query, text=True, capture_output=True, check=True).stdout.strip()
    device_id, gb_id = literal(args.device_id), literal(args.gb_device_id)
    channel_id, gb_channel = literal(args.channel_id), literal(args.gb_channel_id)
    identities = sql(f"SELECT count(*) FROM devices d JOIN channels c ON c.did=d.id WHERE d.id={device_id} AND d.device_id={gb_id} AND c.id={channel_id} AND c.device_id={gb_id} AND c.channel_id={gb_channel};")
    if identities != '1':
        raise ValueError('设备/通道身份不唯一或不匹配，停止恢复')
    records = json.loads(sql(f"SELECT coalesce(json_agg(t ORDER BY t.id),'[]'::json) FROM gb_task_states t WHERE kind='alarm_inbox' AND device_id={gb_id} AND payload::jsonb->>'last_error'='awaiting source retransmission before business commit';"))
    selected = []
    for record in records:
        state = json.loads(record['payload'])
        alarm = state['event']
        if alarm['device_id'] != args.gb_device_id or alarm['channel_id'] != args.gb_channel_id:
            raise ValueError('旧任务存在其他目标，停止恢复')
        when = dt.datetime.fromisoformat(alarm['alarm_time'])
        if not when.tzinfo:
            when = when.replace(tzinfo=start.tzinfo)
        if not start <= when <= end:
            continue
        if alarm.get('cmd_type') != 'Alarm' or alarm.get('source_method') not in ('MESSAGE', 'NOTIFY') or not re.fullmatch(r'[a-f0-9]{64}', alarm['delivery_id']) or alarm['delivery_id'] != record['session_id']:
            raise ValueError('旧任务投递身份不合法')
        if state.get('business_committed_at', '').split('T')[0] not in ('', '0001-01-01'):
            raise ValueError('目标含已提交记录，需独立核对')
        selected.append((record, alarm, when))
    preview = {'selected': len(selected), 'device_id': args.device_id, 'channel_id': args.channel_id, 'start': args.start, 'end': args.end, 'realtime_notifications': False}
    print(json.dumps(preview, ensure_ascii=False), flush=True)
    if not args.apply:
        return
    if not selected:
        raise ValueError('没有可恢复记录')
    os.umask(0o077)
    backup = pathlib.Path(args.backup_dir)
    backup.mkdir(mode=0o700, parents=True, exist_ok=False)
    raw = json.dumps([r for r, _, _ in selected], ensure_ascii=False, indent=2) + '\n'
    (backup / 'legacy-alarms.json').write_text(raw)
    keys = ','.join(literal(a['delivery_id']) for _, a, _ in selected)
    before = sql(f"SELECT coalesce(json_agg(e),'[]'::json) FROM events e WHERE source_key IN ({keys});")
    (backup / 'events-before.json').write_text(before + '\n')
    existing = json.loads(before)
    if any(e['did'] != args.device_id or e['cid'] != args.channel_id for e in existing):
        raise ValueError('现有事件幂等键身份冲突，停止恢复')
    values = []
    for record, alarm, when in selected:
        snapshot = dict(alarm, internal_device_id=args.device_id, internal_channel_id=args.channel_id, historical_recovery={'legacy_task_id': record['id'], 'received_at': json.loads(record['payload'])['received_at']})
        values.append('(' + ','.join([device_id, channel_id, literal(when.isoformat())+'::timestamptz', literal((when+dt.timedelta(seconds=1)).isoformat())+'::timestamptz', literal('gb_alarm_'+alarm['alarm_method']), '1', literal(json.dumps(snapshot, ensure_ascii=False)), "''", "'GB28181'", literal(alarm['delivery_id']), 'now()', 'now()']) + ')')
    # 此专用历史入口直接写表，避免触发应用层实时推送、上级转发和声音联动。
    query = f"""BEGIN;
SET LOCAL lock_timeout='5s';
SET LOCAL statement_timeout='30s';
SET LOCAL TIME ZONE 'Asia/Shanghai';
DO $$ BEGIN
 PERFORM d.id FROM devices d JOIN channels c ON c.did=d.id
 WHERE d.id={device_id} AND d.device_id={gb_id} AND c.id={channel_id} AND c.device_id={gb_id} AND c.channel_id={gb_channel}
 FOR SHARE OF d,c;
 IF NOT FOUND THEN RAISE EXCEPTION 'recovery identity changed'; END IF;
END $$;
WITH inserted AS (
 INSERT INTO events(did,cid,started_at,ended_at,label,score,zones,image_path,model,source_key,created_at,updated_at)
 VALUES {','.join(values)} ON CONFLICT(source_key) DO NOTHING RETURNING id
) SELECT count(*) FROM inserted;
COMMIT;"""
    (backup / 'recovery.sql').write_text(query)
    result = sql(query).splitlines()
    after = json.loads(sql(f"SELECT coalesce(json_agg(json_build_object('id',id,'source_key',source_key,'did',did,'cid',cid)),'[]'::json) FROM events WHERE source_key IN ({keys});"))
    if len(after) != len(selected):
        raise RuntimeError('恢复后事件数量不匹配，保留备份供核对')
    report = dict(preview, inserted=int(result[-1]), existing=len(existing), recovered=len(after), backup_sha256=hashlib.sha256(raw.encode()).hexdigest(), events=after)
    (backup / 'recovery-report.json').write_text(json.dumps(report, ensure_ascii=False, indent=2)+'\n')
    print(json.dumps({k: v for k, v in report.items() if k != 'events'}, ensure_ascii=False))


if __name__ == '__main__':
    main()
