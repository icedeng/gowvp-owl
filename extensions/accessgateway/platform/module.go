// Package platform 是 A 端模块的装配入口；不持有原项目的 Usecase。
package platform

import (
	"context"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/credentials"
	"github.com/gowvp/owl/extensions/accessgateway/platform/httpapi"
	"github.com/gowvp/owl/extensions/accessgateway/platform/runtime"
	"github.com/gowvp/owl/extensions/accessgateway/platform/service"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"gorm.io/gorm"
)

type Module struct {
	Service    *service.Service
	Runtime    *runtime.Manager
	files      *credentials.Files
	once       sync.Once
	closeError error
}

func New(ctx context.Context, config core.Config, db *gorm.DB, signal core.SignalHost, management core.ManagementHost, bind func(*runtime.Manager)) (*Module, error) {
	m := &Module{}
	if e := config.Validate(); e != nil {
		return nil, e
	}
	if !config.Enabled {
		return m, nil
	}
	files, e := credentials.Open(config.CredentialDir)
	if e != nil {
		return nil, e
	}
	m.files = files
	repo := store.New(db)
	m.Runtime = runtime.New(ctx, config, repo, files, signal)
	if bind != nil {
		bind(m.Runtime)
	}
	m.Service = service.New(ctx, config, repo, m.Runtime, files, management)
	if e = m.Runtime.Start(); e != nil {
		_ = m.Close()
		return nil, e
	}
	m.Service.Start()
	return m, nil
}
func (m *Module) Register(router gin.IRouter, admin gin.HandlerFunc, actor func(*gin.Context) string) {
	httpapi.New(m.Service, actor).Register(router, admin)
}
func (m *Module) Close() error {
	m.once.Do(func() {
		if m.Service != nil {
			_ = m.Service.Close()
		}
		if m.Runtime != nil {
			_ = m.Runtime.Close()
		}
		if m.files != nil {
			m.closeError = m.files.Close()
		}
	})
	return m.closeError
}
