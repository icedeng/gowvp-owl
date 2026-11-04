import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";
import ts from "typescript";
const { outputText } = ts.transpileModule(
  readFileSync(new URL("../src/utils/recordings.ts", import.meta.url), "utf8"),
  { compilerOptions: { module: ts.ModuleKind.ESNext } }
);
const {
  calendarCells,
  recordingDateRange,
  recordingMediaPath,
  recordingTime,
  localDateInput,
} = await import(
  `data:text/javascript;base64,${Buffer.from(outputText).toString("base64")}`
);

test("完整月历保留月份边界，周一起始，支持闰年和跨年", () => {
  const november = calendarCells("2026-11");
  assert.equal(november[6], "2026-11-01");
  assert.equal(november[8], "2026-11-03");
  assert.equal(november.filter(Boolean).length, 30);
  assert.equal(calendarCells("2024-02").filter(Boolean).length, 29);
  assert.ok(calendarCells("2026-12").includes("2026-12-31"));
  assert.equal(calendarCells("2027-01").filter(Boolean).length, 31);
});
test("日期范围按本地自然日/月，包含末毫秒且不越界", () => {
  const day = recordingDateRange("2026-11-03");
  assert.equal(new Date(day.start_ms).getHours(), 0);
  assert.equal(localDateInput(new Date(day.end_ms)), "2026-11-03");
  const month = recordingDateRange("2026-12", true);
  assert.equal(localDateInput(new Date(month.start_ms)), "2026-12-01");
  assert.equal(localDateInput(new Date(month.end_ms)), "2026-12-31");
  assert.equal(localDateInput(new Date(month.end_ms + 1)), "2027-01-01");
  assert.equal(
    recordingTime("2026-11-03 14:39:19"),
    new Date(2026, 10, 3, 14, 39, 19).getTime()
  );
});
test("回放编码原始文件路径，拒绝目录穿越和外部地址", () => {
  assert.equal(
    recordingMediaPath("/rtp/ch1/2026-11-03/a b#?.mp4"),
    "/static/recordings/rtp/ch1/2026-11-03/a%20b%23%3F.mp4"
  );
  for (const path of [
    "",
    "../file.mp4",
    "rtp/../file.mp4",
    "rtp/./file.mp4",
    "https://other/file.mp4",
    "rtp\\file.mp4",
  ])
    assert.throws(() => recordingMediaPath(path));
});
