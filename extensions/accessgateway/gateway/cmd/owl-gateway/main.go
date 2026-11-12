package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/app"
	"github.com/gowvp/owl/extensions/accessgateway/gateway/internal/config"
	"github.com/gowvp/owl/extensions/accessgateway/protocol"
)

var gatewayVersion = "dev"
var sourceCommit = "unknown"

func main() {
	protocol.GatewayBuild = gatewayVersion + "+" + sourceCommit
	path := flag.String("config", "/etc/owl-gateway/gateway.toml", "网关 TOML 配置路径")
	version := flag.Bool("version", false, "显示构建版本")
	flag.Parse()
	if *version {
		fmt.Printf("owl-gateway %s source_commit=%s protocol_version=1 compatible=1\n", gatewayVersion, sourceCommit)
		return
	}
	c, e := config.Load(*path)
	if e != nil {
		slog.Error("网关配置加载失败", "error", e)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if e = app.Run(ctx, c); e != nil {
		slog.Error("网关运行失败", "error", e)
		os.Exit(1)
	}
}
