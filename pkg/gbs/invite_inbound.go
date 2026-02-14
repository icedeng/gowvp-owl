package gbs

import "github.com/gowvp/owl/pkg/gbs/sip"

// sipInviteGeneric 处理入向 INVITE（9.2 被叫侧基础兼容）。
// 当前实现返回 200 并回送 SDP，满足上级平台会话建立要求。
func (g *GB28181API) sipInviteGeneric(ctx *sip.Context) {
	respBody := ctx.Request.Body()
	resp := sip.NewResponseFromRequest("", ctx.Request, 200, "OK", respBody)
	if len(respBody) > 0 {
		resp.AppendHeader(&sip.ContentTypeSDP)
	}
	_ = ctx.Tx.Respond(resp)
}

// sipByeGeneric 处理入向 BYE，会话释放返回 200。
func (g *GB28181API) sipByeGeneric(ctx *sip.Context) {
	ctx.String(200, "OK")
}

// sipAckGeneric 处理入向 ACK，ACK 本身不需要应答。
func (g *GB28181API) sipAckGeneric(_ *sip.Context) {}
