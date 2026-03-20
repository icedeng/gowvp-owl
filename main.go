package main

import (
	"expvar"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gowvp/owl/internal/app"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/system"
)

// @title GoWVP Owl API
// @version 0.0.1
// @description GoWVP Owl 的 HTTP API 文档，包含设备、通道、GB28181、PTZ、录像与系统接口。
// @description
// @description 鉴权方式：
// @description 大多数管理接口需要在请求头中携带 `Authorization: Bearer <token>`。
// @description 可先调用 `/login/key` 获取 RSA 公钥，再调用 `/login` 完成登录。
// @description
// @description 返回约定：
// @description 1. 成功时通常返回业务对象，或 `{ "msg": "ok" }`
// @description 2. 失败时通常返回 `{ "msg": "错误原因" }`
// @description 3. GB28181 查询类接口通常会同时返回结构化结果与原始 XML
// @BasePath /
// @securityDefinitions.apikey BearerAuth
// @in header
// @name Authorization
// @tag.name System
// @tag.description 系统运行状态、版本检查、升级、运行指标等平台级接口。
// @tag.name Auth
// @tag.description 登录、公钥获取、用户凭据修改等认证接口。
// @tag.name Config
// @tag.description 平台配置查看与修改接口，当前重点包含 SIP 配置。
// @tag.name MediaServer
// @tag.description 流媒体服务器管理与 HTTP 代理接口。
// @tag.name Device
// @tag.description 设备管理接口，覆盖 GB28181、ONVIF 等多协议设备。
// @tag.name Channel
// @tag.description 通道管理、播放、抓拍、录像模式、AI 控制等接口。
// @tag.name GB28181
// @tag.description GB/T 28181 相关接口，包含统一控制、统一查询、订阅、校时、抓拍回调等。
// @tag.name PTZ
// @tag.description 云台 PTZ 控制接口，统一兼容 GB28181 和 ONVIF 常用能力。
// @tag.name Recording
// @tag.description 本地录像查询、时间轴、月度统计、下载与 HLS 播放列表接口。
// @tag.name AIWebhook
// @tag.description AI 分析服务回调接口，用于接收 AI 心跳、启动、检测结果、停止通知。
// @tag.name ZLMWebhook
// @tag.description ZLMediaKit 或兼容流媒体服务的回调接口。
// @tag.name ONVIF
// @tag.description ONVIF 发现与相关能力接口。
// @tag.name Event
// @tag.description AI 或业务事件查询、详情、图片访问接口。

var (
	buildVersion = "0.0.1" // 构建版本号
	gitBranch    = "dev"   // git 分支
	gitHash      = "debug" // git 提交点哈希值
	release      string    // 发布模式 true/false
	buildTime    string    // 构建时间戳
)

// 自定义配置目录
var configDir = flag.String("conf", "./configs", "config directory, eg: -conf /configs/")

func getBuildRelease() bool {
	v, _ := strconv.ParseBool(release)
	return v
}

func main() {
	flag.Parse()

	// 初始化配置
	var bc conf.Bootstrap
	filedir, _ := system.Abs(*configDir)
	_ = os.MkdirAll(filedir, 0o755)
	filePath := filepath.Join(filedir, "config.toml")

	configIsNotExistWrite(filePath)
	if err := conf.SetupConfig(&bc, filePath); err != nil {
		panic(err)
	}
	bc.Debug = !getBuildRelease()
	bc.BuildVersion = buildVersion
	bc.ConfigDir = filedir
	bc.ConfigPath = filePath

	{
		expvar.NewString("version").Set(buildVersion)
		expvar.NewString("git_branch").Set(gitBranch)
		expvar.NewString("git_hash").Set(gitHash)
		expvar.NewString("build_time").Set(buildTime)
		expvar.Publish("timestamp", expvar.Func(func() any {
			return time.Now().Format(time.DateTime)
		}))
	}

	app.Run(&bc)
}

// configIsNotExistWrite 配置文件不存在时，回写配置
func configIsNotExistWrite(path string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := conf.WriteConfig(conf.DefaultConfig(), path); err != nil {
			system.ErrPrintf("WriteConfig err[%s]", err)
		}
	}
}
