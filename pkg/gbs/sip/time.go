package sip

import "time"

var gbTimeLocation = time.FixedZone("CST", 8*60*60)

// GBTimeLocation 返回 GB/T 28181 规定的北京时间（UTC+8）。
func GBTimeLocation() *time.Location {
	return gbTimeLocation
}

// FormatGBTime 按北京时间格式化协议时间。
func FormatGBTime(value time.Time, layout string) string {
	return value.In(gbTimeLocation).Format(layout)
}

// ParseGBTime 按北京时间解析不带时区的协议时间；输入自带时区时遵循输入时区。
func ParseGBTime(layout, value string) (time.Time, error) {
	return time.ParseInLocation(layout, value, gbTimeLocation)
}
