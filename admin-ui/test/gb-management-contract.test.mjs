import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const readView = (name) => readFileSync(
  new URL(`../src/views/${name}`, import.meta.url),
  "utf8",
);

const capabilityView = readView("GBProtocolCapabilitiesView.vue");
const detailView = readView("DeviceDetailView.vue");
const diagnosticsView = readView("DiagnosticsView.vue");
const sipSettingsView = readView("SipSettingsView.vue");
const routerSource = readFileSync(
  new URL("../src/router/index.ts", import.meta.url),
  "utf8",
);
const swaggerModels = readFileSync(
  new URL("../../internal/web/api/swagger_models.go", import.meta.url),
  "utf8",
);

test("附录 G 审计切换使用请求序列隔离并提供完整 Tab 语义", () => {
  assert.match(capabilityView, /let auditLoadSequence = 0/);
  assert.match(capabilityView, /sequence !== auditLoadSequence \|\| tab !== auditTab\.value/);
  assert.match(capabilityView, /role="tablist"/);
  assert.match(capabilityView, /role="tabpanel"/);
  assert.equal((capabilityView.match(/aria-controls="audit-panel"/g) || []).length, 3);
  assert.match(capabilityView, /id="audit-panel"[^>]+role="tabpanel"/);
  assert.match(capabilityView, /event\.key === "ArrowRight"/);
});

test("附录 G 关闭时不触发不可用审计接口并隐藏无效查询控件", () => {
  assert.match(capabilityView, /if \(annexStatus\.value === "disabled"\)/);
  assert.match(capabilityView, /auditError\.value = ""/);
  assert.match(capabilityView, /await loadSummary\(\);\s+await loadAudits\(\);/);
  assert.match(capabilityView, /v-if="annexStatus !== 'disabled'"/);
  assert.match(capabilityView, /审计存储随附录 G 运行时启用/);
  assert.doesNotMatch(capabilityView, /仍可查询既有审计/);
});

test("国标能力页展示上级级联的逐平台状态与版本协商结果", () => {
  assert.match(capabilityView, /上级平台级联状态/);
  assert.match(capabilityView, /cascadeStatuses\.value = cascade\.status === "fulfilled"/);
  assert.match(capabilityView, /item\.negotiated_version \|\| item\.configured_version/);
  assert.match(capabilityView, /cascadeStateLabel/);
  assert.match(capabilityView, /last_error/);
  assert.match(capabilityView, /to="\/sip-settings#upstreams"/);
});

test("国标运行摘要区分加载、成功与失败状态并提供克制刷新反馈", () => {
  assert.match(capabilityView, /type SummaryState = "idle" \| "loading" \| "ready" \| "error"/);
  assert.match(capabilityView, /summaryNote\(summaryStates\.cascade, `\$\{configuredVersions\} 个版本档案已配置`\)/);
  assert.match(capabilityView, /return annexEnabled\.value \? "enabled" : "disabled"/);
  assert.match(capabilityView, /const annexStatus = computed\(\(\) =>/);
  assert.match(capabilityView, /annexStatus === 'unknown'/);
  assert.match(capabilityView, /summaryStates\.metrics === "ready"/);
  assert.match(capabilityView, /summaryRevision\.value \+= 1/);
  assert.match(capabilityView, /runtime-strip\.is-updated/);
  assert.match(capabilityView, /prefers-reduced-motion: reduce/);
  assert.doesNotMatch(capabilityView, /metrics\.annex_g_pending \|\| 0/);
});

test("移动端能力矩阵保留首列上下文并提供 44 像素审计控件", () => {
  assert.match(capabilityView, /@media \(max-width: 600px\) \{ \.runtime-strip \{ grid-template-columns: repeat\(2, minmax\(0, 1fr\)\)/);
  assert.match(capabilityView, /\.capability-table tr > :first-child \{ position: sticky; left: 0/);
  assert.match(capabilityView, /\.audit-pagination \.page-btn \{ width: 44px; height: 44px/);
  assert.match(capabilityView, /\.segmented button \{ flex: 1 0 auto; min-height: 44px/);
});

test("四版本兼容对讲与 2016+ 标准双流程门禁保持一致", () => {
  assert.match(capabilityView, /2011: verify\("兼容对讲已实现", "UDP 双向音频与厂商差异待互通"\)/);
  assert.match(detailView, /"1\.0": new Set\(\["directory_notify", "media_status", "voice_intercom"\]\)/);
  assert.match(detailView, /voice_intercom: "2011\+（标准双流程为 2016\+）"/);
  assert.match(readView("ChannelDetailView.vue"), /supportsStandardVoiceTalk/);
  assert.match(readView("ChannelDetailView.vue"), /标准双流程语音对讲仅支持 2016\/2022/);
});

test("附录 G 配置锚点在异步加载后滚动并聚焦", () => {
  assert.match(routerSource, /scrollBehavior\(to, _from, savedPosition\)/);
  assert.match(routerSource, /if \(to\.hash\) return \{ el: to\.hash, top: 80 \}/);
  assert.match(sipSettingsView, /async function focusRouteSection\(\)/);
  assert.match(sipSettingsView, /target\.scrollIntoView\(\{ block: "start" \}\)/);
  assert.match(sipSettingsView, /target\.focus\(\{ preventScroll: true \}\)/);
  assert.match(sipSettingsView, /await focusRouteSection\(\)/);
});

test("附录 G 启动配置只读展示并提供可执行的配置文件路径与复制片段", () => {
  assert.match(sipSettingsView, /只读启动配置/);
  assert.match(sipSettingsView, /<fieldset class="static-config" disabled>/);
  assert.match(sipSettingsView, /\.\/configs\/config\.toml/);
  assert.match(sipSettingsView, /copyAnnexGConfig/);
  assert.match(sipSettingsView, /复制 TOML 片段/);
  assert.match(sipSettingsView, /replace-with-current-secret/);
  assert.doesNotMatch(sipSettingsView, /@click="addAnnexGSystem"/);
  assert.doesNotMatch(sipSettingsView, /@click="removeAnnexGSystem/);
  assert.doesNotMatch(sipSettingsView, /\n\s+annex_g:\s*\{/);
  assert.doesNotMatch(sipSettingsView, /annex_g_passwords:/);
});

test("设备操作按有效能力与 2022 档案门禁", () => {
  assert.match(detailView, /basicConfigAvailability = computed\(\(\) => capabilityAvailability\("config_write"\)\)/);
  assert.match(detailView, /capability: "mobile_position"/);
  assert.match(detailView, /capability: "ptz_position"/);
  assert.match(detailView, /!basicConfigAvailability\.available/);
  assert.match(detailView, /!a4Availability\.available/);
});

test("主动校时明确标注为厂商扩展并说明标准校时路径", () => {
  assert.match(detailView, /<strong>厂商扩展校时<\/strong>/);
  assert.match(detailView, /标准校时由 REGISTER 200 OK Date 或 NTP 完成/);
  assert.doesNotMatch(detailView, /<strong>设备校时<\/strong>/);
});

test("设备事件订阅页面覆盖创建、续订、取消及后端公开过滤参数", () => {
  assert.match(detailView, /event: "alarm" as SubscriptionEvent/);
  for (const field of [
    "target_id",
    "event",
    "expires",
    "start_alarm_priority",
    "end_alarm_priority",
    "alarm_method",
    "alarm_type",
    "start_alarm_time",
    "end_alarm_time",
    "start_time",
    "end_time",
    "interval",
  ]) {
    assert.match(detailView, new RegExp(`${field}:`), `订阅请求缺少 ${field}`);
  }
  assert.match(detailView, /api\.subscribe\(device\.value!\.id, body\)/);
  assert.match(detailView, /api\.subscribe\(device\.value!\.id, \{ \.\.\.receipt\.payload, cancel: true \}\)/);
  assert.match(detailView, /api\.subscriptionStates\(id\)/);
  assert.match(detailView, /subscriptionPayloadFromState/);
  assert.match(detailView, /await loadSubscriptionStates\(\)/);
  assert.match(detailView, /currentSubscriptionReceipt \? "续订" : "创建订阅"/);
  assert.match(detailView, /服务端订阅运行态/);
  assert.match(detailView, /订阅意图会持久保存/);
  assert.match(detailView, /服务重启或设备重新注册后建立新对话/);
  assert.match(detailView, /recovering: "待恢复"/);
  assert.match(detailView, /取消订阅/);
  assert.doesNotMatch(detailView, /刷新后无法恢复/);
  assert.match(detailView, /normalizeSubscriptionTime/);
});

test("设备详情在能力字段缺失时按四版本回退，显式空数组仍保持严格", () => {
  assert.match(detailView, /"1\.0": new Set\(\["directory_notify", "media_status", "voice_intercom"\]\)/);
  assert.match(detailView, /"1\.1": new Set\(\[[\s\S]*?"config_write"/);
  assert.match(detailView, /"2\.0": new Set\(\[[\s\S]*?"mobile_position"/);
  assert.match(detailView, /"3\.0": new Set\(\[[\s\S]*?"ptz_position"/);
  assert.match(detailView, /Array\.isArray\(value\) \? value : undefined/);
  assert.match(detailView, /declaredCapabilities\.value \|\| fallbackCapabilitiesByVersion/);
  assert.match(detailView, /effectiveCapabilities\.size \? 'ready' : 'pending'/);
  assert.match(detailView, /项按版本推断/);
});

test("设备高级配置页覆盖后端公开的全部 DeviceConfig 扩展节点", () => {
  const model = swaggerModels.match(/type SwaggerGBDeviceConfigInput struct \{([\s\S]*?)\n\}/);
  assert.ok(model, "未找到 SwaggerGBDeviceConfigInput");
  const publicSections = [...model[1].matchAll(/json:"([^",]+)/g)]
    .map((match) => match[1])
    .filter((key) => !["target_id", "timeout", "extra_info", "basic_param"].includes(key))
    .sort();
  const options = detailView.match(/const advancedConfigOptions = \[([\s\S]*?)\n\] as const;/);
  assert.ok(options, "未找到 advancedConfigOptions");
  const pageSections = [...options[1].matchAll(/key: "([^"]+)"/g)]
    .map((match) => match[1])
    .sort();
  assert.deepEqual(pageSections, publicSections);
});

test("高级配置校验 JSON 对象并按动态节点提交，抓拍配置追加能力门禁", () => {
  assert.match(detailView, /payload = JSON\.parse\(advancedConfigForm\.payload\)/);
  assert.match(detailView, /Array\.isArray\(payload\) \|\| typeof payload !== "object"/);
  assert.match(detailView, /\[advancedConfigForm\.section\]: payload/);
  assert.match(detailView, /key: "snapshot_config"[\s\S]*?capability: "snapshot"/);
  assert.match(detailView, /if \("capability" in option && option\.capability\) return capabilityAvailability\(option\.capability\)/);
});

test("诊断页区分配置查询写入以及禁用和未声明状态", () => {
  assert.match(diagnosticsView, /\["BasicParam 查询", "2014\+", "config_query"\]/);
  assert.match(diagnosticsView, /\["BasicParam 写入", "2014\+", "config_write"\]/);
  assert.match(diagnosticsView, /disabledCapabilities\.value\.has\(key\)/);
  assert.match(diagnosticsView, /\? "已禁用"/);
  assert.match(diagnosticsView, /declaredCapabilities\.value \? "未声明" : "不支持"/);
});

test("管理端区分 REGISTER 绑定关闭与有效绑定内离线", () => {
  const devicesView = readView("DevicesView.vue");
  assert.match(devicesView, /gb_registration_closed === true/);
  assert.match(devicesView, /绑定已关闭/);
  assert.match(devicesView, /设备已注销或 REGISTER 有效期已结束/);
  assert.match(devicesView, /注册绑定仍有效/);
  assert.match(detailView, /注册绑定已关闭/);
  assert.match(detailView, /设备已注销或 REGISTER 有效期已结束/);
  assert.match(detailView, /REGISTER 绑定仍有效，请检查心跳或 DeviceStatus/);
  assert.match(detailView, /:class="registrationState\.tone">\{\{ registrationState\.badge \}\}/);
  assert.match(diagnosticsView, /REGISTER 绑定/);
  assert.match(diagnosticsView, /有效但离线/);
  assert.match(diagnosticsView, /diagnostics-comparison \{ align-items: start; \}/);
  assert.match(detailView, /gb_version_updated_at \? formatDate/);
  assert.match(diagnosticsView, /gb_version_updated_at \? formatDate/);
});

test("2014 裸 TCP 设备落盘与级联中继可独立配置并共享安全边界", () => {
  assert.match(sipSettingsView, /const directDownloadConfigValid = computed/);
  assert.match(sipSettingsView, /!directDownload\.enabled && !directDownload\.cascade_relay_enabled/);
  assert.match(sipSettingsView, /isIPLiteral\(directDownload\.relay_listen_ip\)/);
  assert.match(sipSettingsView, /!isUnspecifiedIP\(directDownload\.relay_advertise_ip\)/);
  assert.match(sipSettingsView, /\^\\d\{20\}\$/);
  assert.match(sipSettingsView, /directDownload\.device_concurrency <= directDownload\.global_concurrency/);
  assert.match(sipSettingsView, /directDownload\.total_timeout_seconds >= value/);
  assert.match(sipSettingsView, /const directCIDRError = computed/);
  assert.match(sipSettingsView, /new Set\(cidrs\)\.size !== cidrs\.length/);
  assert.match(sipSettingsView, /!isCIDRLiteral\(value\)/);
  assert.match(sipSettingsView, /\^\(0\|\[1-9\]\\d\{0,2\}\)\$/);
  assert.match(sipSettingsView, /aria-invalid="Boolean\(directCIDRError\)"/);
  assert.match(sipSettingsView, /aria-describedby="direct-cidr-help direct-cidr-error direct-download-error"/);
  assert.match(sipSettingsView, /id="direct-cidr-error"/);
  assert.match(sipSettingsView, /relay_port_end >= directDownload\.relay_port_start/);
  assert.match(sipSettingsView, /cascade_relay_enabled: directDownload\.cascade_relay_enabled/);
  assert.match(sipSettingsView, /启用设备直连落盘/);
  assert.match(sipSettingsView, /启用上级平台裸 TCP 级联/);
  assert.match(sipSettingsView, /!directDownloadConfigValid/);
  assert.match(capabilityView, /设备直连落盘与上级平台流式中继/);
  assert.match(capabilityView, /内置下载客户端与级联中继已有本地自动化证据；真实设备和上级平台互通待验收/);
  assert.match(sipSettingsView, /media_allowed_cidrs/);
  assert.match(sipSettingsView, /if \(loadError\.value\)/);
  assert.match(sipSettingsView, /Boolean\(loadError\)/);
  assert.match(sipSettingsView, /\.form-grid \.input, \.form-grid \.select \{ width: 100%; min-width: 0; \}/);
  for (const field of ["cascade_relay_enabled", "relay_listen_ip", "relay_advertise_ip", "relay_port_start", "relay_port_end"]) {
  assert.match(swaggerModels, new RegExp(`json:"${field}"`));
  }
  assert.match(sipSettingsView, /Owl 内置附录 O 的 TCP 下载客户端/);
  assert.match(sipSettingsView, /本入口承接规范消息 3/);
});
