package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/notify"
	"github.com/ixugo/goddd/pkg/web"
	"github.com/ixugo/goddd/pkg/ws"
)

// wsHub 全局 WebSocket Hub 实例，供各模块广播告警
var wsHub *adminSockets

// 每个会话代次使用独立 Hub，关闭整个旧 Hub 也会拦住正在握手的旧连接。
type adminSockets struct {
	mu     sync.RWMutex
	hub    ws.Huber
	create func() ws.Huber
}

func (s *adminSockets) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.create == nil {
		return
	}
	old := s.hub
	s.hub = s.create()
	if old != nil {
		old.Close()
	}
}

func (s *adminSockets) Broadcast(message ws.Message) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.hub != nil {
		s.hub.Broadcast(message)
	}
}

func (s *adminSockets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.hub.ServeHTTP(w, r)
}

func (s *adminSockets) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hub != nil {
		s.hub.Close()
	}
}

// broadcastWarn 向所有已认证的 WebSocket 客户端广播一条通用警告通知
func broadcastWarn(msg string) {
	if wsHub == nil {
		return
	}
	wsHub.Broadcast(ws.NewMessage("warn", map[string]string{"msg": msg}))
}

// broadcastIPCWarn 向所有已认证的 WebSocket 客户端广播一条设备相关警告通知
func broadcastIPCWarn(msg, deviceID, name string) {
	if wsHub == nil {
		return
	}
	wsHub.Broadcast(ws.NewMessage("ipc_warn", map[string]string{"msg": msg, "device_id": deviceID, "name": name}))
}

// broadcastIPCInfo 向所有已认证的 WebSocket 客户端广播一条设备相关信息通知
func broadcastIPCInfo(msg, deviceID, name string) {
	if wsHub == nil {
		return
	}
	wsHub.Broadcast(ws.NewMessage("ipc_info", map[string]string{"msg": msg, "device_id": deviceID, "name": name}))
}

// registerWS 注册 WebSocket 路由并初始化 Hub
func registerWS(r *gin.Engine, uc *Usecase) {
	sockets := uc.UserAPI.sockets
	sockets.create = func() ws.Huber { return newAdminHub(uc) }
	sockets.reset()
	r.GET("/ws", func(c *gin.Context) { sockets.ServeHTTP(c.Writer, c.Request) })
	wsHub = sockets
	notify.SetWarnFunc(broadcastWarn)
	notify.SetIPCWarnFunc(broadcastIPCWarn)
	notify.SetIPCInfoFunc(broadcastIPCInfo)
}

func newAdminHub(uc *Usecase) ws.Huber {
	hub := ws.NewHub(func(c *ws.Config) {
		c.MaxConnections = 128
	})

	hub.SetAuthHandler(func(message ws.Message) (string, error) {
		var data struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(message.Data(), &data); err != nil {
			return "", fmt.Errorf("invalid auth message")
		}
		if data.Token == "" {
			return "", fmt.Errorf("token is empty")
		}
		claims, err := web.ParseToken(data.Token, uc.Conf.Server.HTTP.JwtSecret)
		if err != nil {
			return "", fmt.Errorf("invalid token")
		}
		if !uc.validAdminSession(claims) {
			return "", fmt.Errorf("token expired or revoked")
		}
		userID, _ := claims.Data[web.KeyUsername].(string)
		return userID, nil
	})

	hub.SetConnectHandler(func(client *ws.Client) error {
		// HTTP 升级请求返回后其 context 会取消；握手提示使用独立的有界上下文。
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return client.Send(ctx, ws.NewMessage("auth", nil))
	})

	hub.SetErrorHandler(func(client *ws.Client, err error) {
		slog.Debug("ws client error", "client_id", client.ID(), "err", err)
	})

	return hub
}
