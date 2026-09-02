import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(
  new URL("../src/views/SipSettingsView.vue", import.meta.url),
  "utf8",
);

test("上级平台 Date + Note seed 按原始字节绑定并提交", () => {
  assert.match(source, /signal_digest_seed:\s*item\.clear_signal_digest_seed/);
  assert.match(source, /v-model="item\.signal_digest_seed"/);
  assert.doesNotMatch(source, /signal_digest_seed:\s*item\.signal_digest_seed\?\.trim\(/);
  assert.doesNotMatch(source, /v-model\.trim="item\.signal_digest_seed"/);
});

test("脱敏后的上级密钥默认保留且只能显式清除", () => {
  assert.match(source, /sipSecrets\.value\s*=\s*data\.sip_secrets\s*\|\|\s*\{\}/);
  assert.match(source, /password:\s*item\.clear_password\s*\|\|\s*item\.password_input\s*===\s*""\s*\?\s*undefined/);
  assert.doesNotMatch(source, /password:\s*item\.password_input\s*\|\|\s*item\.password/);
  assert.match(source, /secret_clears:\s*\{/);
  assert.match(source, /upstream_passwords:/);
  assert.match(source, /upstream_signal_digest_seeds:/);
  assert.match(source, /v-model="item\.clear_password"/);
  assert.match(source, /v-model="item\.clear_signal_digest_seed"/);
});

test("全局信令摘要 seed 留空保留并支持显式清除", () => {
  assert.match(source, /signal_digest_seed_configured/);
  assert.match(source, /seed:\s*clearSignalDigestSeed\.value\s*\|\|\s*!signalDigestSeedInput\.value\s*\?\s*undefined/);
  assert.match(source, /signal_digest_seed:\s*clearSignalDigestSeed\.value/);
  assert.match(source, /v-model="clearSignalDigestSeed"/);
  assert.doesNotMatch(source, /signalDigestSeedInput\.value\.trim\(/);
});

test("附录 G 密码与 seed 仅展示脱敏状态且不进入在线保存", () => {
  assert.match(source, /sipSecrets\.value\.annex_g_systems\?\.\[item\.id\]/);
  assert.match(source, /item\.password_configured \? "replace-with-current-secret"/);
  assert.match(source, /item\.signal_digest_seed_configured/);
  assert.match(source, /<fieldset class="static-config" disabled>/);
  assert.doesNotMatch(source, /annex_g_passwords:/);
  assert.doesNotMatch(source, /annex_g_signal_digest_seeds:/);
  assert.doesNotMatch(source, /\n\s+annex_g:\s*\{/);
});
