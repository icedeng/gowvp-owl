import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const viewSource = readFileSync(
  new URL("../src/views/ChannelDetailView.vue", import.meta.url),
  "utf8",
);
const liveViewSource = readFileSync(
  new URL("../src/views/LiveView.vue", import.meta.url),
  "utf8",
);
const apiSource = readFileSync(
  new URL("../src/services/api.ts", import.meta.url),
  "utf8",
);
const backendApiSource = readFileSync(
  new URL("../../internal/web/api/ipc.go", import.meta.url),
  "utf8",
);

test("通道详情提供国标扩展查询、控制、抓拍和升级入口", () => {
  assert.match(viewSource, /label: "国标扩展"/);
  assert.match(viewSource, /@click="runGBQuery"/);
  assert.match(viewSource, /@click="runGBControl"/);
  assert.match(viewSource, /@click="refreshSnapshot"/);
  assert.match(viewSource, /@click="startUpgrade"/);
});

test("国标查询和控制提交到设备接口并显式携带目标通道", () => {
  assert.match(viewSource, /api\.gbQuery\(device\.value\.id, body\)/);
  assert.match(viewSource, /api\.gbControl\(device\.value\.id, body\)/);
  assert.equal(
    (viewSource.match(/target_id: channel\.value\.channel_id \|\| channel\.value\.id/g) || []).length,
    2,
  );
  assert.match(apiSource, /gbQuery:.*\/devices\/\$\{encodeURIComponent\(id\)\}\/gb\/query/);
  assert.match(apiSource, /gbControl:.*\/devices\/\$\{encodeURIComponent\(id\)\}\/gb\/control/);
});

test("统一控制页面覆盖后端公开的 A.2.3 动作及其结构化参数", () => {
  const documentedActions = backendApiSource
    .match(/`action` 支持：(.+?)。历史/)?.[1]
    .match(/`[a-z0-9_]+`/g)
    ?.map((value) => value.slice(1, -1));
  assert.ok(documentedActions?.length, "未能读取后端公开控制动作清单");
  for (const action of documentedActions) {
    assert.match(viewSource, new RegExp(`<option value="${action}"`), `缺少 ${action} 控制入口`);
  }
  assert.match(viewSource, /body\.ptz_cmd = gbControlForm\.ptzCmd/);
  assert.match(viewSource, /body\.stream_number = gbControlForm\.streamNumber/);
  assert.match(viewSource, /body\.drag_zoom = area/);
  assert.match(viewSource, /body\.home_position = gbControlForm\.homeEnabled === 0/);
  assert.match(viewSource, /body\.target_track = \{/);
  assert.match(viewSource, /min="0" \/><small>0 表示格式化全部存储卡/);
});

test("2022 SnapShot 配置查询同时受 snapshot 能力门禁", () => {
  assert.match(viewSource, /value="snapshot" :disabled="!hasGBCapability\('snapshot'\)"/);
  assert.match(viewSource, /gbQueryForm\.configType !== "snapshot" \|\| hasGBCapability\("snapshot"\)/);
});

test("厂商兼容报警筛选请求明确区别于四版本标准订阅并按版本门禁 AlarmType", () => {
  assert.match(viewSource, /<option value="alarm">报警筛选请求（厂商兼容）<\/option>/);
  assert.match(viewSource, /标准报警筛选与持续接收请在设备详情使用 9\.11 报警订阅/);
  assert.doesNotMatch(viewSource, /<option value="alarm">报警查询（四版本）<\/option>/);
  for (const field of [
    "startAlarmPriority",
    "endAlarmPriority",
    "alarmMethod",
    "alarmType",
    "startAlarmTime",
    "endAlarmTime",
  ]) {
    assert.match(viewSource, new RegExp(`gbQueryForm\\.${field}`), `报警查询表单缺少 ${field}`);
  }
  assert.match(viewSource, /alarmTypeQuerySupported = computed/);
  assert.match(viewSource, /:disabled="!alarmTypeQuerySupported"/);
  assert.match(viewSource, /body\.start_alarm_priority = gbQueryForm\.startAlarmPriority\.trim\(\)/);
  assert.match(viewSource, /body\.end_alarm_priority = gbQueryForm\.endAlarmPriority\.trim\(\)/);
  assert.match(viewSource, /body\.alarm_method = gbQueryForm\.alarmMethod\.trim\(\)/);
  assert.match(viewSource, /body\.alarm_type = alarmTypeQuerySupported\.value \? gbQueryForm\.alarmType\.trim\(\) : ""/);
  assert.match(viewSource, /body\.start_alarm_time = gbQueryForm\.startAlarmTime\.trim\(\)/);
  assert.match(viewSource, /body\.end_alarm_time = gbQueryForm\.endAlarmTime\.trim\(\)/);
  assert.match(viewSource, /起始报警级别不能高于结束级别/);
  assert.match(viewSource, /起始报警时间不能晚于结束时间/);
});

test("A.2.4 标准设备信息、目录和录像查询在管理端可用", () => {
  for (const action of ["device_info", "catalog", "record_info"]) {
    assert.match(viewSource, new RegExp(`<option value="${action}">`), `缺少 ${action} 查询入口`);
  }
  for (const field of [
    "startAt",
    "endAt",
    "filePath",
    "address",
    "secrecy",
    "recordType",
    "recorderId",
    "indistinctQuery",
    "streamNumber",
  ]) {
    assert.match(viewSource, new RegExp(`gbQueryForm\\.${field}`), `标准查询表单缺少 ${field}`);
  }
  assert.match(viewSource, /body\.start = start/);
  assert.match(viewSource, /body\.end = end/);
  assert.match(viewSource, /body\.file_path = gbQueryForm\.filePath\.trim\(\)/);
  assert.match(viewSource, /body\.indistinct_query = Number\(gbQueryForm\.indistinctQuery\)/);
  assert.match(viewSource, /body\.stream_number = Number\(gbQueryForm\.streamNumber\)/);
  assert.match(viewSource, /录像查询结束时间必须晚于起始时间/);
  assert.match(viewSource, /2016\/2022 的录像查询必须填写起止时间/);
});

test("抓拍和升级会话均可查询最终状态", () => {
  assert.match(apiSource, /snapshotState:.*\/snapshot\/\$\{encodeURIComponent\(sessionId\)\}\/status/);
  assert.match(apiSource, /upgradeDeviceState:.*\/upgrade\/\$\{encodeURIComponent\(sessionId\)\}/);
  assert.match(viewSource, /api\.snapshotState\(channel\.value\.id, snapshotSessionId\.value\)/);
  assert.match(viewSource, /api\.upgradeDeviceState\(channel\.value\.id, upgradeSessionId\.value\)/);
});

test("历史会话覆盖四版本 RTP 与 2014 附录 O 下载闭环", () => {
  assert.match(viewSource, /@click="startHistorySession"/);
  assert.match(viewSource, /@click="controlHistorySession"/);
  assert.match(viewSource, /@click="stopHistorySession"/);
  assert.match(viewSource, /value="direct_tcp" :disabled="!supportsDirectDownload"/);
  assert.match(viewSource, /api\.directDownloadState\(historyState\.value\.session_id\)/);
  assert.match(viewSource, /api\.cancelDirectDownload\(historyState\.value\.session_id\)/);
  assert.match(apiSource, /directDownloadState:.*\/gb28181\/downloads\/\$\{encodeURIComponent\(sessionId\)\}/);
  assert.match(apiSource, /cancelDirectDownload:.*\/gb28181\/downloads\/\$\{encodeURIComponent\(sessionId\)\}/);
  assert.match(viewSource, /void restoreHistoryState\(data\.id, sequence\)/);
  assert.match(viewSource, /if \(state\.transport === "direct_tcp" \|\| state\.transport === "rtp"\) historyForm\.transport = state\.transport/);
  assert.match(backendApiSource, /DirectTCPDownloadByChannel\(device\.DeviceID, channel\.ChannelID\)/);
  assert.match(backendApiSource, /Transport: "direct_tcp"/);
});

test("历史下载校验倍速并正确表达未知进度", () => {
  assert.match(viewSource, /Number\.isInteger\(speed\) && speed >= 1 && speed <= 16/);
  assert.match(viewSource, /hasGBCapability\("download_speed"\) \? Number\(historyForm\.downloadSpeed\) : 1/);
  assert.match(viewSource, /!historyStartAllowed/);
  assert.match(viewSource, /:aria-valuenow="historyProgressKnown \? historyProgress : undefined"/);
  assert.match(viewSource, /:aria-valuetext="historyProgressKnown \? `\$\{historyProgress\.toFixed\(1\)\}%` : historyStatusLabel"/);
});

test("历史回放支持四版本负倍率以及 2011/2022 指定起点", () => {
  assert.match(viewSource, /min="-16" max="16" step="0\.25"/);
  assert.match(viewSource, /scale !== 0 && Math\.abs\(scale\) >= 0\.25 && Math\.abs\(scale\) <= 16/);
  assert.match(viewSource, /effectiveGbVersion\.value === "1\.0" \|\| effectiveGbVersion\.value === "3\.0" && Number\(historyForm\.scale\) < 0/);
  assert.match(viewSource, /supportsPositionedSpeed\.value && historyForm\.speedAt \? \{ seek_at: historyUnix\(historyForm\.speedAt\) \} : \{\}/);
  assert.match(viewSource, /按 2011 生成 Scale 与 npt Range/);
  assert.match(viewSource, /从指定位置倒放至录像起点/);
});

test("升级普通字符串仅裁剪判空，提交时保留原始值", () => {
  assert.match(viewSource, /!upgradeForm\.firmware\.trim\(\)/);
  assert.match(viewSource, /firmware: upgradeForm\.firmware/);
  assert.match(viewSource, /file_url: upgradeForm\.fileUrl/);
  assert.match(viewSource, /manufacturer: upgradeForm\.manufacturer/);
  assert.doesNotMatch(viewSource, /firmware:\s*upgradeForm\.firmware\.trim\(\)/);
  assert.doesNotMatch(viewSource, /file_url:\s*upgradeForm\.fileUrl\.trim\(\)/);
  assert.doesNotMatch(viewSource, /manufacturer:\s*upgradeForm\.manufacturer\.trim\(\)/);
  assert.doesNotMatch(viewSource, /v-model\.trim="upgradeForm\.(?:firmware|fileUrl|manufacturer)"/);
});

test("能力字段存在时严格使用声明，缺失时按版本档案回退", () => {
  assert.match(viewSource, /Array\.isArray\(deviceCapabilities\)/);
  assert.match(viewSource, /hasDeclaredGBCapabilities = computed\(\(\) => gbCapabilityDeclaration\.value !== undefined\)/);
  assert.match(viewSource, /if \(hasDeclaredGBCapabilities\.value\) return gbCapabilities\.value\.includes\(name\)/);
  assert.match(viewSource, /device\.value\?\.ext\?\.gb_version/);
  assert.match(viewSource, /channel\.value\?\.ext\?\.gb_version/);
  assert.match(viewSource, /case "2011-supplement-2014":/);
  assert.match(viewSource, /return "1\.1"/);
  assert.match(viewSource, /\["1\.1", "2\.0", "3\.0"\]\.includes\(version\)/);
  assert.doesNotMatch(viewSource, /\["1\.1", "2\.0", "3\.0", "2014", "2016", "2022"\]\.includes\(version\)/);
});

test("四版本基础查询和控制不依赖扩展能力数组", () => {
  assert.doesNotMatch(viewSource, /device_status:\s*"[a-z0-9_]+"/);
  assert.doesNotMatch(viewSource, /(?:teleboot|guard_set|guard_reset):\s*"[a-z0-9_]+"/);
  assert.match(viewSource, /!queryCapability\.value \|\| hasGBCapability\(queryCapability\.value\)/);
  assert.match(viewSource, /const controlAvailable = computed\(\(\) => !controlCapability\.value \|\|/);
});

test("2016 与 2022 管理端显式提供标准双流程语音对讲", () => {
  assert.match(backendApiSource, /talk_standard 按 GB\/T 28181-2016\/2022/);
  assert.match(viewSource, /mode: "talk_standard" as "talk_standard" \| "talk" \| "broadcast"/);
  assert.match(viewSource, /<option value="talk_standard"[^>]*>标准对讲（2016\/2022）<\/option>/);
  assert.match(viewSource, /<option value="talk"[^>]*>兼容对讲（2011\+ 单 INVITE）<\/option>/);
  assert.match(viewSource, /:disabled="!supportsStandardVoiceTalk"/);
  assert.match(viewSource, /api\.voiceStart\(channel!\.id, \{ mode: voiceForm\.mode/);
  assert.match(viewSource, /api\.voiceStop\(channel!\.id, \{ mode: voiceForm\.mode \}\)/);

  // 实时预览页无法提供必填音频源时，应进入完整语音配置，不得发送必然失败的空源请求。
  assert.match(liveViewSource, /router\.push\(\{ name: "channel-detail", params: \{ id: selectedChannel\.value\.id \} \}\)/);
  assert.doesNotMatch(liveViewSource, /api\.voiceStart\(selectedChannel!\.id, \{ mode: 'talk' \}\)/);
});
