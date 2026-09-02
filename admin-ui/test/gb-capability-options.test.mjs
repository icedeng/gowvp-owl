import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const viewSource = readFileSync(
  new URL("../src/views/DeviceDetailView.vue", import.meta.url),
  "utf8",
);
const diagnosticsSource = readFileSync(
  new URL("../src/views/DiagnosticsView.vue", import.meta.url),
  "utf8",
);
const devicesSource = readFileSync(
  new URL("../src/views/DevicesView.vue", import.meta.url),
  "utf8",
);
const backendSource = readFileSync(
  new URL("../../pkg/gbs/version.go", import.meta.url),
  "utf8",
);

function extractQuotedKeys(source, startMarker, endMarker) {
  const start = source.indexOf(startMarker);
  const end = source.indexOf(endMarker, start);
  assert.notEqual(start, -1, `未找到 ${startMarker}`);
  assert.notEqual(end, -1, `未找到 ${endMarker}`);
  return new Set([...source.slice(start, end).matchAll(/"([a-z0-9_]+)"/g)].map((match) => match[1]));
}

test("设备能力管理项覆盖后端全部已知能力键", () => {
  const frontendKeys = extractQuotedKeys(viewSource, "const gbCapabilityOptions", "] as const;");
  const backendKeys = extractQuotedKeys(
    backendSource,
    "var knownGBCapabilityNames",
    "// NormalizeGBDisabledCapabilities",
  );
  assert.deepEqual([...frontendKeys].sort(), [...backendKeys].sort());
});

test("协议诊断矩阵覆盖后端全部已知能力键", () => {
  const frontendKeys = extractQuotedKeys(diagnosticsSource, "const capabilityDefinitions", "] as const;");
  const backendKeys = extractQuotedKeys(
    backendSource,
    "var knownGBCapabilityNames",
    "// NormalizeGBDisabledCapabilities",
  );
  assert.deepEqual([...frontendKeys].sort(), [...backendKeys].sort());
});

test("协议诊断仅在能力字段缺失时按四版本档案回退", () => {
  assert.match(diagnosticsSource, /Array\.isArray\(value\) \? value : undefined/);
  assert.match(diagnosticsSource, /declaredCapabilities\.value \|\| fallbackCapabilitiesByVersion/);
  assert.match(diagnosticsSource, /"1\.0": new Set\(\["directory_notify", "media_status", "voice_intercom"\]\)/);
  assert.match(diagnosticsSource, /"1\.1": new Set\(\[[\s\S]*?"config_write"/);
  assert.match(diagnosticsSource, /"2\.0": new Set\(\[[\s\S]*?"mobile_position"/);
  assert.match(diagnosticsSource, /"3\.0": new Set\(\[[\s\S]*?"ptz_position"/);
});

test("2014 与 2016 能力的最低版本提示保持准确", () => {
  assert.match(viewSource, /drag_zoom_control:\s*"2014\+"/);
  assert.match(viewSource, /home_position:\s*"2016\+"/);
  assert.match(viewSource, /home_position_query:\s*"仅 2022"/);
  assert.match(diagnosticsSource, /\["语音对讲", "2011\+（标准双流程为 2016\+）", "voice_intercom"\]/);
});

test("国标设备列表提供四版本筛选并兼容旧版分页接口", () => {
  assert.match(devicesSource, /const version = ref\("all"\)/);
  assert.match(devicesSource, /function protocolVersion\(device: ApiDevice\)/);
  assert.match(devicesSource, /2011（1\.0）/);
  assert.match(devicesSource, /2014（1\.1）/);
  assert.match(devicesSource, /2016（2\.0）/);
  assert.match(devicesSource, /2022（3\.0）/);
  assert.match(devicesSource, /watch\(\[query, status, version\]/);
  assert.match(devicesSource, /collectPages\(api\.devices, \{ type: "GB28181" \}\)/);
});
