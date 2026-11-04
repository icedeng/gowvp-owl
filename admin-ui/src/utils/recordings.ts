/** 将服务本地时间转换为兼容各浏览器的时间格式。 */
export function recordingTime(value?: string) {
  return value ? new Date(value.replace(" ", "T")).getTime() : NaN;
}
export function localDateInput(value: Date) {
  return `${value.getFullYear()}-${String(value.getMonth() + 1).padStart(
    2,
    "0"
  )}-${String(value.getDate()).padStart(2, "0")}`;
}
export function calendarCells(month: string): (string | null)[] {
  const first = new Date(`${month}-01T00:00:00`);
  const offset = (first.getDay() + 6) % 7;
  const count = new Date(
    first.getFullYear(),
    first.getMonth() + 1,
    0
  ).getDate();
  return Array.from({ length: 42 }, (_, index) => {
    const day = index - offset + 1;
    return day > 0 && day <= count
      ? `${month}-${String(day).padStart(2, "0")}`
      : null;
  });
}
export function recordingDateRange(value: string, wholeMonth = false) {
  const start = new Date(`${wholeMonth ? `${value}-01` : value}T00:00:00`);
  const end = new Date(start);
  if (wholeMonth) end.setMonth(end.getMonth() + 1);
  else end.setDate(end.getDate() + 1);
  return { start_ms: start.getTime(), end_ms: end.getTime() - 1 };
}
/** 编码各路径段，保留目录结构；不接受父目录、绝对地址及反斜杠。 */
export function recordingMediaPath(path: string) {
  const segments = path.replace(/^\/+/, "").split("/");
  if (
    segments.some(
      (part) => !part || part === "." || part === ".." || /[\\:]/.test(part)
    )
  )
    throw new Error("录像文件路径无效");
  return `/static/recordings/${segments.map(encodeURIComponent).join("/")}`;
}
