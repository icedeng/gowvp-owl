package gbs

import "fmt"

const (
	gbVersion2011 = "2011"
	gbVersion2016 = "2016"
	gbVersion2022 = "2022"
)

// getDeviceGBVersion 获取设备国标版本；未知时按 2016 处理。
func (g *GB28181API) getDeviceGBVersion(deviceID string) string {
	if d, ok := g.svr.memoryStorer.Load(deviceID); ok {
		switch d.GBVersion() {
		case gbVersion2011, gbVersion2016, gbVersion2022:
			return d.GBVersion()
		}
	}
	return gbVersion2016
}

// requireGBVersionAtLeast 检查设备是否满足最小协议版本要求。
func (g *GB28181API) requireGBVersionAtLeast(deviceID string, min string, feature string) error {
	ver := g.getDeviceGBVersion(deviceID)
	rank := map[string]int{
		gbVersion2011: 1,
		gbVersion2016: 2,
		gbVersion2022: 3,
	}
	if rank[ver] < rank[min] {
		return fmt.Errorf("%s 在 GB/T 28181-%s 中未定义，当前设备版本为 %s", feature, min, ver)
	}
	return nil
}
