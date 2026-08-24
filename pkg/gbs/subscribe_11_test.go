package gbs

import (
	"context"
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

func TestCatalogSubscriptionEventValueByVersion(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		want    string
	}{
		{version: GBVersion10, want: "presence"},
		{version: GBVersion11, want: "Catalog;id=" + gb10DeviceID},
		{version: GBVersion20, want: "Catalog;id=" + gb10DeviceID},
		{version: GBVersion30, want: "Catalog;id=" + gb10DeviceID},
	}

	for _, tt := range tests {
		t.Run(string(tt.version), func(t *testing.T) {
			if got := buildSubscriptionEventValueForVersion(tt.version, "Catalog", gb10DeviceID); got != tt.want {
				t.Fatalf("Catalog Event = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestBasicSubscriptionEventValueUsesPresence(t *testing.T) {
	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		for _, cmdType := range []string{"Alarm", "MobilePosition", "PTZPosition"} {
			if got := buildSubscriptionEventValueForVersion(version, cmdType, gb10DeviceID); got != "presence" {
				t.Fatalf("version %s %s Event = %q; want presence", version, cmdType, got)
			}
		}
	}
}

func TestSubscriptionEventHeaderMustMatchBody(t *testing.T) {
	if err := validateSubscriptionEventHeader("presence", "Alarm", "", gb10DeviceID); err != nil {
		t.Fatalf("standard Alarm presence Event rejected: %v", err)
	}
	if err := validateSubscriptionEventHeader("Catalog;id="+gb10DeviceID, "Catalog", gb10DeviceID, gb10DeviceID); err != nil {
		t.Fatalf("standard Catalog Event rejected: %v", err)
	}
	if err := validateSubscriptionEventHeader("Catalog;id="+gb10DeviceID, "Alarm", gb10DeviceID, gb10DeviceID); err == nil {
		t.Fatal("Catalog Event accepted for Alarm subscription body")
	}
	if err := validateSubscriptionEventHeader("Alarm;id="+gb10DeviceID, "Alarm", gb10DeviceID, gb10DeviceID); err == nil {
		t.Fatal("non-Catalog subscription accepted Event id")
	}
}

func TestNormalizeSubscribeCmdType(t *testing.T) {
	tests := map[string]string{
		"alarm":           "Alarm",
		"mobile_position": "MobilePosition",
		"device-position": "MobilePosition",
		"ptz_position":    "PTZPosition",
		"Catalog":         "Catalog",
	}
	for input, want := range tests {
		got, ok := normalizeSubscribeCmdType(input)
		if !ok || got != want {
			t.Fatalf("normalizeSubscribeCmdType(%q) = %q, %v; want %q, true", input, got, ok, want)
		}
	}
	if got, ok := normalizeSubscribeCmdType("Alarm\r\nX-Injected: true"); ok || got != "" {
		t.Fatalf("header injection event accepted: %q, %v", got, ok)
	}
}

func TestOutgoingSubscriptionRenewalReusesDialog(t *testing.T) {
	conn := newFlowConnection()
	initial := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-dialog", []byte("query"))
	response := sip.NewResponseFromRequest("", initial, 200, "OK", nil)
	to, ok := response.To()
	if !ok || to == nil {
		t.Fatal("initial response missing To")
	}
	if to.Params == nil {
		to.Params = sip.NewParams()
	}
	to.Params.Add("tag", sip.String{Str: "remote-tag"})
	remoteTarget := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.99:5070")
	response.AppendHeader(&sip.ContactHeader{Address: remoteTarget.URI.Clone(), Params: sip.NewParams()})

	dialog := &outgoingSubscriptionDialog{
		response:     response,
		remoteTarget: remoteTarget.URI.Clone(),
		eventValue:   "Catalog;id=" + gb10DeviceID,
	}
	renewal := newFlowRequest(t, conn, sip.MethodSubscribe, "different-dialog", []byte("query"))
	applyOutgoingSubscriptionDialog(renewal, dialog)

	callID, ok := renewal.CallID()
	if !ok || normalizeCallID(callID) != "subscribe-dialog" {
		t.Fatalf("renewal Call-ID = %v", callID)
	}
	cseq, ok := renewal.CSeq()
	if !ok || cseq.SeqNo != 2 || cseq.MethodName != sip.MethodSubscribe {
		t.Fatalf("renewal CSeq = %+v", cseq)
	}
	renewalTo, ok := renewal.To()
	if !ok || renewalTo.Params == nil {
		t.Fatal("renewal missing remote To tag")
	}
	tag, ok := renewalTo.Params.Get("tag")
	if !ok || tag.String() != "remote-tag" {
		t.Fatalf("renewal To tag = %v", tag)
	}
	if renewal.Recipient().String() != remoteTarget.URI.String() {
		t.Fatalf("renewal target = %s; want %s", renewal.Recipient(), remoteTarget.URI)
	}
}

func TestOutgoingSubscriptionCancelRequiresDialog(t *testing.T) {
	memory := newFlowMemory(gb10DeviceID)
	api := &GB28181API{svr: &Server{memoryStorer: memory}}
	err := api.Subscribe(t.Context(), &SubscribeInput{DeviceID: gb10DeviceID, Event: "catalog", Cancel: true})
	if err == nil || !strings.Contains(err.Error(), "subscription does not exist") {
		t.Fatalf("cancel missing subscription error = %v", err)
	}
}

func TestTerminatedNotifyClearsOutgoingSubscription(t *testing.T) {
	api := &GB28181API{}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil)}
	api.outgoingSubscriptions.Store("catalog-key", dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-subscription", nil)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("terminated-notify", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	assertFlowOK(t, <-flowResponse(t, connection))
	if _, exists := api.outgoingSubscriptions.Load("catalog-key"); exists {
		t.Fatal("terminated NOTIFY left outgoing subscription dialog")
	}
}

func TestTerminatedNotifyRenewsReferencedCascadeSubscription(t *testing.T) {
	api := &GB28181API{cascadeSubscriptions: make(map[string]*cascadeDownstreamSubscription)}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-cascade-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil)}
	key := "cascade-catalog-key"
	api.outgoingSubscriptions.Store(key, dialog)
	input := SubscribeInput{DeviceID: gb10DeviceID, TargetID: gb10DeviceID, Event: "Catalog", Expires: 60}
	api.cascadeSubscriptions[key] = &cascadeDownstreamSubscription{Input: input, Refs: 1}
	renewed := make(chan SubscribeInput, 1)
	api.cascadeSubscribe = func(_ context.Context, actual *SubscribeInput) error {
		renewed <- *actual
		return nil
	}

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-cascade-subscription", nil)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("terminated-cascade-notify", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	assertFlowOK(t, <-flowResponse(t, connection))
	select {
	case actual := <-renewed:
		if actual.DeviceID != input.DeviceID || actual.TargetID != input.TargetID || actual.Event != input.Event || actual.Cancel {
			t.Fatalf("renewed cascade subscription = %+v", actual)
		}
	case <-time.After(time.Second):
		t.Fatal("terminated referenced cascade subscription was not renewed")
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
	initialResponse := <-flowResponse(t, conn)
	assertFlowOK(t, initialResponse)
	if !strings.Contains(initialResponse, "Event: Catalog;id="+gb10DeviceID) || !strings.Contains(initialResponse, "Expires: 60") {
		t.Fatalf("initial SUBSCRIBE response missing dialog headers:\n%s", initialResponse)
	}

	var key string
	var firstExpiry time.Time
	var firstSubscription *eventSubscription
	api.eventSubscribers.Range(func(storedKey, value any) bool {
		key = storedKey.(string)
		sub := value.(*eventSubscription)
		firstSubscription = sub
		sub.CSeq = 7
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
	renewed := value.(*eventSubscription)
	if renewed != firstSubscription || renewed.CSeq != 7 {
		t.Fatalf("subscription renewal replaced dialog state: first=%p renewed=%p CSeq=%d", firstSubscription, renewed, renewed.CSeq)
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

func TestCatalogSubscriptionResponse10IncludesBusinessAck(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>51</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-10", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-10-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion10),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	for _, required := range []string{"Event: presence", "Expires: 90", "<Response>", "<Result>OK</Result>"} {
		if !strings.Contains(response, required) {
			t.Fatalf("1.0 SUBSCRIBE response missing %q:\n%s", required, response)
		}
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
