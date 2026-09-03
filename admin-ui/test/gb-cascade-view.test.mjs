import test from "node:test";
import assert from "node:assert/strict";
import fs from "node:fs";
import path from "node:path";

const root = path.resolve(new URL("..", import.meta.url).pathname);
const read = (file) => fs.readFileSync(path.join(root, "src", file), "utf8");

test("级联平台页面提供四版本状态矩阵和运行时筛选", () => {
  const view = read("views/GBCascadeView.vue");
  assert.match(view, /api\.cascadeStatuses\(\)/);
  assert.match(view, /2011（1\.0）/);
  assert.match(view, /2014（1\.1）/);
  assert.match(view, /2016（2\.0）/);
  assert.match(view, /2022（3\.0）/);
  assert.match(view, /按注册状态筛选/);
  assert.match(view, /:aria-pressed="statusFilter === 'all'"/);
  assert.match(view, /最近注册/);
  assert.match(view, /三级链路验收/);
  assert.match(view, /运行状态（待真实验收）/);
});

test("级联平台页面区分接口失败、未启用和保留旧快照", () => {
  const view = read("views/GBCascadeView.vue");
  assert.match(view, /const hasLoaded = ref\(false\)/);
  assert.match(view, /loadError && !hasLoaded/);
  assert.match(view, /级联状态暂不可用/);
  assert.match(view, /尚未启用上级平台/);
  assert.match(view, /当前继续显示最近一次成功结果/);
  assert.match(view, /运行中的上级档案/);
});

test("级联平台页面识别四版本别名和完整注册生命周期", () => {
  const view = read("views/GBCascadeView.vue");
  assert.match(view, /"2011": "1\.0"/);
  assert.match(view, /"2014": "1\.1"/);
  assert.match(view, /"2016": "2\.0"/);
  assert.match(view, /"2022": "3\.0"/);
  assert.match(view, /expired: "注册已过期"/);
  assert.match(view, /retrying: "等待重试"/);
  assert.match(view, /const errorStates = new Set\(\["error", "failed", "expired", "stopped"\]\)/);
  assert.match(view, /function isVersionDowngrade\(item: CascadePlatformStatus\)/);
  assert.match(view, /new Set\(platforms\.value\.map\(\(item\) => normalizeVersion/);
  assert.match(view, /:title="item\.last_error"/);
});

test("级联平台页面已接入导航、命令面板和能力矩阵入口", () => {
  const router = read("../src/router/index.ts");
  const shell = read("layouts/AppShell.vue");
  const palette = read("components/CommandPalette.vue");
  const capabilities = read("views/GBProtocolCapabilitiesView.vue");
  assert.match(router, /path: 'gb28181-cascade'/);
  assert.match(shell, /name: "gb28181-cascade", label: "级联平台"/);
  assert.match(palette, /path: '\/gb28181-cascade'/);
  assert.match(capabilities, /to="\/gb28181-cascade"/);
});
