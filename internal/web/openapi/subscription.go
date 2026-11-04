package openapi

import (
	"bytes"
	"encoding/json"

	"github.com/gin-gonic/gin"
	op "github.com/gowvp/owl/internal/core/openplatform"
)

func (a *API) subscriptionRoutes(g *gin.RouterGroup) {
	check := func(c *gin.Context) bool { return require(c, a.S, "event:subscribe") }
	g.GET("/subscriptions", func(c *gin.Context) {
		if !check(c) {
			return
		}
		resourcePage[op.Subscription](a, c, a.S.DB.Model(&op.Subscription{}).Where("app_id = ?", principal(c).App.ID), "open_subscriptions", func(v op.Subscription) string { return v.ID })
	})
	g.POST("/subscriptions", func(c *gin.Context) {
		if !check(c) {
			return
		}
		a.saveSubscription(c, true)
	})
	g.GET("/subscriptions/:subscription", func(c *gin.Context) {
		if !check(c) {
			return
		}
		row, e := owned[op.Subscription](a, c, c.Param("subscription"))
		respond(c, row, e)
	})
	g.PATCH("/subscriptions/:subscription", func(c *gin.Context) {
		if !check(c) {
			return
		}
		a.saveSubscription(c, false)
	})
	g.DELETE("/subscriptions/:subscription", func(c *gin.Context) {
		if !check(c) {
			return
		}
		row, e := owned[op.Subscription](a, c, c.Param("subscription"))
		if e != nil {
			respond(c, nil, e)
			return
		}
		if row.Status != "cancelled" {
			row.Status = "cancelled"
			row, e = a.S.SaveSubscription(principal(c), row, false)
		}
		respond(c, row, e)
	})
	g.POST("/subscriptions/:subscription/signing-keys", func(c *gin.Context) {
		if !a.requireSecretTransport(c) {
			return
		}
		if !check(c) {
			return
		}
		id, secret, e := a.S.RotateCallbackKey(principal(c), c.Param("subscription"))
		respond(c, gin.H{"key_id": id, "secret_key": secret}, e)
	})
	g.GET("/subscriptions/:subscription/deliveries", func(c *gin.Context) {
		if !check(c) {
			return
		}
		if _, e := owned[op.Subscription](a, c, c.Param("subscription")); e != nil {
			respond(c, nil, e)
			return
		}
		q := a.S.DB.Model(&op.Delivery{}).Where("app_id = ? AND subscription_id = ?", principal(c).App.ID, c.Param("subscription"))
		if state := c.Query("state"); state != "" {
			q = q.Where("state = ?", state)
		}
		q, e := timeFilter(c, q, "created_at")
		if e != nil {
			respond(c, nil, e)
			return
		}
		resourcePage[op.Delivery](a, c, q, "open_deliveries", func(v op.Delivery) string { return v.ID })
	})
	g.POST("/subscriptions/:subscription/deliveries/:delivery/retry", func(c *gin.Context) {
		if !check(c) {
			return
		}
		id, ok := a.idem(c, "delivery.retry")
		if !ok {
			return
		}
		if id != "" {
			respond(c, gin.H{"delivery_id": id, "queued": true}, nil)
			return
		}
		e := a.S.Retry(principal(c).App.ID, c.Param("subscription"), c.Param("delivery"), "application")
		if e == nil && !a.done(c, "delivery.retry", c.Param("delivery")) {
			return
		}
		respond(c, gin.H{"delivery_id": c.Param("delivery"), "queued": true}, e)
	})
}
func (a *API) saveSubscription(c *gin.Context, create bool) {
	row := op.Subscription{Status: "paused", ChannelIDs: []string{}, Labels: []string{}}
	if !create {
		var e error
		row, e = owned[op.Subscription](a, c, c.Param("subscription"))
		if e != nil {
			respond(c, nil, e)
			return
		}
	}
	var values map[string]json.RawMessage
	if e := Body(c, &values); e != nil {
		respond(c, nil, e)
		return
	}
	for k := range values {
		if !op.Has([]string{"callback_target_id", "channel_ids", "channel_codes", "labels", "status", "expires_at", "revision"}, k) {
			respond(c, nil, op.Fail(400, "UNKNOWN_FIELD", "不支持的订阅字段"))
			return
		}
	}
	if raw, exists := values["channel_codes"]; exists {
		if _, mixed := values["channel_ids"]; mixed {
			respond(c, nil, op.Fail(400, "INVALID_RESOURCE_SELECTOR", "channel_codes 与 channel_ids 不能同时传入"))
			return
		}
		var codes []struct {
			DeviceCode  string `json:"device_code"`
			ChannelCode string `json:"channel_code"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&codes); err != nil || codes == nil || len(codes) > 10000 {
			respond(c, nil, op.Fail(400, "INVALID_FILTER", "channel_codes 须为最多 10000 项的设备编码和完整通道编码数组；空数组表示全部授权通道"))
			return
		}
		ids := []string{}
		seen := map[string]bool{}
		for _, code := range codes {
			_, id, err := a.resourceByCodes(principal(c).App, code.DeviceCode, code.ChannelCode, true)
			if err != nil {
				respond(c, nil, err)
				return
			}
			if !seen[id] {
				ids = append(ids, id)
				seen[id] = true
			}
		}
		delete(values, "channel_codes")
		values["channel_ids"], _ = json.Marshal(ids)
	}
	if !create {
		if _, ok := values["revision"]; !ok {
			respond(c, nil, op.Fail(400, "REVISION_REQUIRED", "修改订阅需要revision"))
			return
		}
	}
	b, _ := json.Marshal(values)
	if e := json.Unmarshal(b, &row); e != nil {
		respond(c, nil, op.Fail(400, "INVALID_SUBSCRIPTION", "订阅字段不合法"))
		return
	}
	if create {
		id, ok := a.idem(c, "subscription.create")
		if !ok {
			return
		}
		if id != "" {
			existing, e := owned[op.Subscription](a, c, id)
			respond(c, existing, e)
			return
		}
	}
	row, e := a.S.SaveSubscription(principal(c), row, create)
	if e == nil && create && !a.done(c, "subscription.create", row.ID) {
		return
	}
	respond(c, row, e)
}
