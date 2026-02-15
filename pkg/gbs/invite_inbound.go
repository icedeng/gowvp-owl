package gbs

import (
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type inboundInviteDialog struct {
	CallID      string
	DeviceID    string
	Established bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// sipInviteGeneric 处理入向 INVITE（9.2 被叫侧基础兼容）。
// 状态机：
// 1) INVITE：创建/刷新会话并返回 200（回送 SDP）；
// 2) ACK：标记会话建立；
// 3) BYE：仅允许已建立会话，成功后返回 200 并删除会话。
func (g *GB28181API) sipInviteGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(400, "missing call-id")
		return
	}

	now := time.Now()
	if v, ok := g.inviteDialogs.Load(callID); ok {
		if d, ok := v.(*inboundInviteDialog); ok && d != nil {
			d.UpdatedAt = now
			g.inviteDialogs.Store(callID, d)
		}
	} else {
		g.inviteDialogs.Store(callID, &inboundInviteDialog{
			CallID:    callID,
			DeviceID:  strings.TrimSpace(ctx.DeviceID),
			CreatedAt: now,
			UpdatedAt: now,
		})
	}

	respBody := ctx.Request.Body()
	resp := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", respBody)
	if len(respBody) > 0 {
		resp.AppendHeader(&sip.ContentTypeSDP)
	}
	// INVITE 200 应包含 Contact，提升与上级平台互通性。
	if g.svr != nil {
		resp.AppendHeader(&sip.ContactHeader{
			DisplayName: g.svr.fromAddress.DisplayName,
			Address:     g.svr.fromAddress.URI.Clone(),
			Params:      g.svr.fromAddress.Params.Clone(),
		})
	}
	_ = ctx.Tx.Respond(resp)
}

// sipByeGeneric 处理入向 BYE。
// 若会话不存在或未建立，返回 481（Call/Transaction Does Not Exist）。
func (g *GB28181API) sipByeGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		ctx.String(400, "missing call-id")
		return
	}
	v, ok := g.inviteDialogs.Load(callID)
	if !ok {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	d, _ := v.(*inboundInviteDialog)
	if d == nil || !d.Established {
		ctx.String(481, "Call/Transaction Does Not Exist")
		return
	}
	ctx.String(200, "OK")
	g.inviteDialogs.Delete(callID)
}

// sipAckGeneric 处理入向 ACK，标记会话为已建立。
func (g *GB28181API) sipAckGeneric(ctx *sip.Context) {
	callID := callIDFromRequest(ctx.Request)
	if callID == "" {
		return
	}
	v, ok := g.inviteDialogs.Load(callID)
	if !ok {
		return
	}
	d, _ := v.(*inboundInviteDialog)
	if d == nil {
		return
	}
	d.Established = true
	d.UpdatedAt = time.Now()
	g.inviteDialogs.Store(callID, d)
}

func callIDFromRequest(req *sip.Request) string {
	if req == nil {
		return ""
	}
	callID, ok := req.CallID()
	if !ok || callID == nil {
		return ""
	}
	return strings.TrimSpace(callID.String())
}

// startInviteDialogCleaner 定时回收长期未更新会话，避免异常场景导致内存堆积。
func (g *GB28181API) startInviteDialogCleaner() {
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		expireBefore := time.Now().Add(-10 * time.Minute)
		g.inviteDialogs.Range(func(key, value any) bool {
			d, ok := value.(*inboundInviteDialog)
			if !ok || d == nil || d.UpdatedAt.Before(expireBefore) {
				g.inviteDialogs.Delete(key)
			}
			return true
		})
	}
}
