package gbs

import (
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestCatalogSubscriptionEventValue11(t *testing.T) {
	value := buildSubscriptionEventValue("Catalog", gb10DeviceID)
	if value != "Catalog;id="+gb10DeviceID {
		t.Fatalf("Catalog Event = %q", value)
	}
	parsed, id, err := parseSubscriptionEvent(value)
	if err != nil || parsed != value || id != gb10DeviceID {
		t.Fatalf("parseSubscriptionEvent() = %q, %q, %v", parsed, id, err)
	}
}

func TestCatalogSubscriptionInitialRenewCancel11(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>50</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)

	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	initialFrom, _ := req.From()
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	tx := sip.NewTransaction("subscribe-1-tx", conn)
	ctx := &sip.Context{
		Request:  req,
		Tx:       tx,
		DeviceID: gb10PlatformID,
		Source:   conn.remote,
		To:       mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"),
		XGBVer:   string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))

	var key string
	var firstExpiry time.Time
	api.eventSubscribers.Range(func(storedKey, value any) bool {
		key = storedKey.(string)
		sub := value.(*eventSubscription)
		firstExpiry = sub.ExpiresAt
		if sub.Event != "Catalog;id="+gb10DeviceID {
			t.Errorf("stored Event = %q", sub.Event)
		}
		return false
	})
	if key == "" {
		t.Fatal("initial subscription was not stored")
	}

	req = newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	req.RemoveHeader("From")
	req.AppendHeader(initialFrom.Clone())
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "120"})
	ctx.Request = req
	ctx.Tx = sip.NewTransaction("subscribe-renew-tx", conn)
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))
	value, ok := api.eventSubscribers.Load(key)
	if !ok || !value.(*eventSubscription).ExpiresAt.After(firstExpiry) {
		t.Fatal("subscription renewal did not extend expiry")
	}

	req = newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-1", body)
	req.RemoveHeader("From")
	req.AppendHeader(initialFrom.Clone())
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "0"})
	ctx.Request = req
	ctx.Tx = sip.NewTransaction("subscribe-cancel-tx", conn)
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))
	if _, ok := api.eventSubscribers.Load(key); ok {
		t.Fatal("subscription cancel did not remove state")
	}
}

func flowResponse(t *testing.T, conn *flowConnection) <-chan string {
	t.Helper()
	out := make(chan string, 1)
	select {
	case payload := <-conn.writes:
		out <- string(payload)
	case <-time.After(time.Second):
		t.Fatal("SIP response timeout")
	}
	return out
}

func TestCatalogSubscriptionRejectsMismatchedEventID11(t *testing.T) {
	_, id, err := parseSubscriptionEvent("Catalog;id=34020000001320009999")
	if err != nil || !strings.HasSuffix(id, "9999") {
		t.Fatalf("unexpected Event parse: id=%q err=%v", id, err)
	}
}
