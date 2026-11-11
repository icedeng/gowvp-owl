package api

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/core/event"
	"github.com/ixugo/goddd/pkg/reason"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
	"gorm.io/gorm"
)

type eventPolicyInput struct {
	DID string `uri:"did" json:"-" binding:"required"`
	event.Policy
}

func (a EventAPI) eventWorkflow() (*event.Workflow, error) {
	w := a.eventCore.Workflow()
	if w == nil {
		return nil, reason.ErrServer.WithMsg("事件工作流不可用")
	}
	return w, nil
}
func (a EventAPI) getEventPolicy(c *gin.Context, _ *struct{}) (*event.Policy, error) {
	w, err := a.eventWorkflow()
	if err != nil {
		return nil, err
	}
	if err = w.CheckDevice(c.Request.Context(), c.Param("did")); err != nil {
		return nil, err
	}
	return w.Policy(c.Request.Context(), c.Param("did"))
}
func (a EventAPI) saveEventPolicy(c *gin.Context, in *eventPolicyInput) (*event.Policy, error) {
	w, err := a.eventWorkflow()
	if err != nil {
		return nil, err
	}
	if err = w.CheckDevice(c.Request.Context(), in.DID); err != nil {
		return nil, err
	}
	if in.Record && a.conf.Server.Recording.Disabled {
		return nil, reason.ErrBadRequest.WithMsg("平台录制已全局禁用，请先在系统配置中启用")
	}
	p := in.Policy
	p.DID = in.DID
	return w.SavePolicy(c.Request.Context(), p, eventActor(c))
}
func eventActor(c *gin.Context) string {
	if s := web.GetUsername(c); s != "" {
		return s
	}
	return "外部鉴权用户"
}
func (a EventAPI) processEvents(c *gin.Context, in *event.ProcessInput) (gin.H, error) {
	w, err := a.eventWorkflow()
	if err != nil {
		return nil, err
	}
	err = w.Process(c.Request.Context(), *in, eventActor(c))
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, reason.ErrNotFound.WithMsg("所选事件已过期或被删除，请刷新")
	}
	return gin.H{"processed": len(in.Items)}, err
}
func (a EventAPI) eventActions(c *gin.Context, in *eventIDInput) (gin.H, error) {
	w, err := a.eventWorkflow()
	if err != nil {
		return nil, err
	}
	if _, err = a.eventCore.GetEvent(c.Request.Context(), in.ID); err != nil {
		return nil, err
	}
	items, err := w.Actions(c.Request.Context(), in.ID)
	return gin.H{"items": items}, err
}
func (a EventAPI) getEventVideo(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		web.Fail(c, reason.ErrBadRequest.WithMsg("事件编号无效"))
		return
	}
	e, err := a.eventCore.GetEvent(c.Request.Context(), id)
	if err != nil {
		web.Fail(c, err)
		return
	}
	path, err := resolveEventMediaPath(e.VideoPath)
	if err != nil {
		web.Fail(c, reason.ErrNotFound.WithMsg("事件视频不存在或已过期"))
		return
	}
	c.Header("Content-Type", "video/mp4")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Cache-Control", "private, no-store")
	if c.Query("download") == "1" {
		c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="event-%d.mp4"`, id))
	}
	http.ServeFile(c.Writer, c.Request, path)
}
func resolveEventMediaPath(relative string) (string, error) {
	if strings.TrimSpace(relative) == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("empty or absolute media path")
	}
	root, err := filepath.EvalSymlinks(filepath.Join(system.Getwd(), "configs", "events"))
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid media path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("media is not a regular file")
	}
	return path, nil
}
