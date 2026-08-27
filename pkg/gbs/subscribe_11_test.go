package gbs

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/m"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

type tcpFlowConnection struct {
	*flowConnection
}

func (*tcpFlowConnection) Network() string { return "tcp" }

func attachFlowEventSubscriptionDialog(t *testing.T, sub *eventSubscription, conn *flowConnection, callID string) {
	t.Helper()
	request := newFlowRequest(t, conn, sip.MethodSubscribe, callID, []byte("query"))
	sub.DialogRequest = request
	sub.Response = sip.NewResponseFromRequest("", request, http.StatusOK, "OK", nil)
}

func TestKeepaliveRespondsBeforeSlowSubscriptionNotify(t *testing.T) {
	baseConn := newFlowConnection()
	conn := &tcpFlowConnection{flowConnection: baseConn}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	memory := newFlowMemory(gb10DeviceID)
	server := &Server{
		Server: sipServer, fromAddress: *platform, memoryStorer: memory,
	}
	api := &GB28181API{svr: server}
	server.gb = api
	statusSubscription := &eventSubscription{
		Key: "slow-device-status", CmdType: "DeviceStatus", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    baseConn.remote, Conn: conn, GBVersion: string(GBVersion10), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, statusSubscription, baseConn, "slow-device-status-dialog")
	api.eventSubscribers.Store("slow-device-status", statusSubscription)

	request := newFlowRequest(t, baseConn, sip.MethodMessage, "keepalive-before-notify", readGB10Fixture(t, "keepalive.xml"))
	request.SetConnection(conn)
	done := make(chan struct{})
	go func() {
		api.sipMessageKeepalive(&sip.Context{
			Request: request, Tx: sip.NewTransaction("keepalive-before-notify-tx", conn),
			DeviceID: gb10DeviceID, Source: baseConn.remote,
			To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
		})
		close(done)
	}()

	var first string
	select {
	case payload := <-baseConn.writes:
		first = string(payload)
	case <-time.After(time.Second):
		t.Fatal("Keepalive response timeout")
	}
	if !strings.Contains(first, "SIP/2.0 200 OK") {
		t.Fatalf("first Keepalive write was delayed by subscription NOTIFY:\n%s", first)
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "NOTIFY ") {
			t.Fatalf("second Keepalive write was not subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("subscription NOTIFY was not sent")
	}
	sipServer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Keepalive handler did not stop after SIP shutdown")
	}
}

func TestAlarmRespondsBeforeSlowSubscriptionNotify(t *testing.T) {
	previousConfig := config
	config = &m.Config{NotifyMap: map[string]string{}}
	t.Cleanup(func() { config = previousConfig })
	baseConn := newFlowConnection()
	conn := &tcpFlowConnection{flowConnection: baseConn}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	server := &Server{Server: sipServer, fromAddress: *platform}
	memory := newFlowMemory(gb10DeviceID)
	memory.runtime.Channels.Store(gb10ChannelID, &Channel{ChannelID: gb10ChannelID, device: memory.runtime})
	server.memoryStorer = memory
	api := &GB28181API{svr: server}
	server.gb = api
	alarmSubscription := &eventSubscription{
		Key: "slow-alarm", CmdType: "Alarm", DeviceID: gb10DeviceID,
		ExpiresAt: time.Now().Add(time.Minute),
		To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
		Source:    baseConn.remote, Conn: conn, GBVersion: string(GBVersion10), Event: "presence",
	}
	attachFlowEventSubscriptionDialog(t, alarmSubscription, baseConn, "slow-alarm-dialog")
	api.eventSubscribers.Store("slow-alarm", alarmSubscription)

	request := newFlowRequest(t, baseConn, sip.MethodMessage, "alarm-before-notify", readGB10Fixture(t, "alarm-notify.xml"))
	request.SetConnection(conn)
	done := make(chan struct{})
	go func() {
		api.sipMessageAlarm(&sip.Context{
			Request: request, Tx: sip.NewTransaction("alarm-before-notify-tx", conn),
			DeviceID: gb10DeviceID, Source: baseConn.remote,
			To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"), Log: slog.Default(),
		})
		close(done)
	}()

	select {
	case payload := <-baseConn.writes:
		if !strings.Contains(string(payload), "SIP/2.0 200 OK") {
			t.Fatalf("first Alarm write was delayed by subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm response timeout")
	}
	select {
	case payload := <-baseConn.writes:
		if !strings.HasPrefix(string(payload), "NOTIFY ") {
			t.Fatalf("second Alarm write was not subscription NOTIFY:\n%s", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("Alarm subscription NOTIFY was not sent")
	}
	sipServer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Alarm handler did not stop after SIP shutdown")
	}
}

func TestDeviceEventNotifyUsesSubscriptionDialogAndSerializesCSeq(t *testing.T) {
	oldConn := newFlowConnection()
	currentBase := newFlowConnection()
	currentConn := &tcpFlowConnection{flowConnection: currentBase}
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	t.Cleanup(sipServer.Close)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api

	subscribe := newFlowRequest(t, oldConn, sip.MethodSubscribe, "device-notify-dialog", []byte("query"))
	contact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@contact.example:5070")
	subscribe.AppendHeader(&sip.ContactHeader{Address: contact.URI.Clone(), Params: sip.NewParams()})
	proxy, _ := sip.ParseURI("sip:proxy.example;lr")
	subscribe.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy}})
	response := sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil)
	sub := &eventSubscription{
		CmdType: "Alarm", DeviceID: gb10DeviceID, Event: "presence",
		ExpiresAt: time.Now().Add(time.Minute), To: contact, Source: currentBase.remote, Conn: currentConn,
		GBVersion: string(GBVersion10), DialogRequest: subscribe, Response: response,
	}
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	errs := make(chan error, 2)
	firstCtx, firstCancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer firstCancel()
	secondCtx, secondCancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer secondCancel()
	go func() { errs <- api.sendEventNotifyContext(firstCtx, sub, "Alarm", body) }()

	requests := make([]string, 0, 2)
	select {
	case payload := <-currentBase.writes:
		requests = append(requests, string(payload))
	case err := <-errs:
		t.Fatalf("first subscription NOTIFY failed before send: %v", err)
	case <-time.After(time.Second):
		t.Fatal("first subscription NOTIFY was not sent")
	}
	go func() { errs <- api.sendEventNotifyContext(secondCtx, sub, "Alarm", body) }()
	select {
	case payload := <-currentBase.writes:
		requests = append(requests, string(payload))
	case <-time.After(time.Second):
		t.Fatal("second subscription NOTIFY was not sent")
	}
	for range 2 {
		if err := <-errs; err == nil {
			t.Fatal("NOTIFY without response unexpectedly succeeded")
		}
	}
	select {
	case payload := <-oldConn.writes:
		t.Fatalf("NOTIFY reused stale subscription connection: %s", payload)
	default:
	}

	for index, request := range requests {
		for _, expected := range []string{
			"NOTIFY sip:" + gb10DeviceID + "@contact.example:5070 SIP/2.0",
			"Route: <sip:proxy.example;lr>",
			"Call-ID: device-notify-dialog",
			fmt.Sprintf("CSeq: %d NOTIFY", index+1),
			"Event: presence",
		} {
			if !strings.Contains(request, expected) {
				t.Fatalf("NOTIFY %d missing %q:\n%s", index+1, expected, request)
			}
		}
	}
}

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
	for _, unsupported := range []string{
		"DeviceInfo", "DeviceStatus", "RecordInfo", "ConfigDownload", "PresetQuery",
		"HomePositionQuery", "CruiseTrackListQuery", "CruiseTrackQuery", "SDCardStatus", "Broadcast", "VendorStatus",
	} {
		if got, ok := normalizeSubscribeCmdType(unsupported); ok || got != "" {
			t.Errorf("non-standard subscription %q accepted as %q", unsupported, got)
		}
	}
}

func TestEventSubscriptionVersionMatrix(t *testing.T) {
	for _, test := range []struct {
		cmdType string
		version GBProtocolVersion
		wantOK  bool
	}{
		{cmdType: "Alarm", version: GBVersion10, wantOK: true},
		{cmdType: "Catalog", version: GBVersion10, wantOK: true},
		{cmdType: "MobilePosition", version: GBVersion11},
		{cmdType: "MobilePosition", version: GBVersion20, wantOK: true},
		{cmdType: "PTZPosition", version: GBVersion20},
		{cmdType: "PTZPosition", version: GBVersion30, wantOK: true},
		{cmdType: "DeviceStatus", version: GBVersion30},
	} {
		t.Run(string(test.version)+"-"+test.cmdType, func(t *testing.T) {
			err := validateSubscribeEventRequest(subscribeEventRequest{SN: 1, DeviceID: gb10DeviceID}, test.cmdType, test.version)
			if test.wantOK && err != nil {
				t.Fatalf("valid subscription rejected: %v", err)
			}
			if !test.wantOK && err == nil {
				t.Fatal("invalid subscription accepted")
			}
		})
	}
}

func TestEventSubscriptionRequiresEnvelopeOnCreateAndCancel(t *testing.T) {
	for _, expires := range []string{"90", "0"} {
		for _, body := range []string{
			`<Query><CmdType>Alarm</CmdType><SN>0</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`,
			`<Query><CmdType>Alarm</CmdType><SN>1</SN></Query>`,
			`<Query><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>invalid</DeviceID></Query>`,
		} {
			api := &GB28181API{}
			conn := newFlowConnection()
			req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-invalid-envelope-"+expires, []byte(body))
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
			ctx := &sip.Context{
				Request: req, Tx: sip.NewTransaction("subscribe-invalid-envelope-tx-"+expires, conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion30),
			}
			api.sipSubscribeEvent(ctx)
			if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 400") {
				t.Fatalf("Expires %s invalid envelope response:\n%s", expires, response)
			}
		}
	}
}

func TestEventSubscriptionRequiresEventHeader(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<Query><CmdType>Catalog</CmdType><SN>53</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-missing-event", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-missing-event-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("missing Event header response:\n%s", response)
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("subscription without Event header was stored")
	}
}

func TestEventSubscriptionRejectsNonStandardQueryCommand(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>DeviceStatus</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-non-event", body)
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
	ctx := &sip.Context{
		Request: req, Tx: sip.NewTransaction("subscribe-non-event-tx", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion30),
	}
	api.sipSubscribeEvent(ctx)
	response := <-flowResponse(t, conn)
	if !strings.Contains(response, "SIP/2.0 400") {
		t.Fatalf("non-standard subscription response:\n%s", response)
	}
	stored := false
	api.eventSubscribers.Range(func(_, _ any) bool {
		stored = true
		return false
	})
	if stored {
		t.Fatal("non-standard subscription was stored")
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
	proxy1, _ := sip.ParseURI("sip:proxy1.example;lr")
	proxy2, _ := sip.ParseURI("sip:proxy2.example;lr")
	response.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy1, proxy2}})

	currentConn := newFlowConnection()
	renewal := newFlowRequest(t, currentConn, sip.MethodSubscribe, "different-dialog", []byte("query"))
	dialogRequest, err := sip.NewRequestFromResponseChecked(sip.MethodSubscribe, response)
	if err != nil {
		t.Fatal(err)
	}
	applyOutgoingSubscriptionDialog(renewal, dialogRequest)

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
	route, ok := renewal.GetHeaders("Route")[0].(*sip.RouteHeader)
	if !ok || len(route.Addresses) != 2 || route.Addresses[0].Host() != "proxy2.example" || route.Addresses[1].Host() != "proxy1.example" {
		t.Fatalf("renewal Route = %#v", renewal.GetHeaders("Route"))
	}
	if renewal.GetConnection() != currentConn {
		t.Fatal("renewal replaced the current connection with the original dialog connection")
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

func TestRefreshInboundSubscriptionDialogPreservesRouteAndUpdatesContact(t *testing.T) {
	conn := newFlowConnection()
	initial := newFlowRequest(t, conn, sip.MethodSubscribe, "refresh-inbound-dialog", []byte("query"))
	oldContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@old-contact.example:5060")
	initial.AppendHeader(&sip.ContactHeader{Address: oldContact.URI.Clone(), Params: sip.NewParams()})
	proxy, _ := sip.ParseURI("sip:proxy.example;lr")
	initial.AppendHeader(&sip.RecordRouteHeader{Addresses: []*sip.URI{proxy}})

	refresh := newFlowRequest(t, conn, sip.MethodSubscribe, "refresh-inbound-dialog", []byte("query"))
	newContact := mustFlowAddress(t, "sip:"+gb10DeviceID+"@new-contact.example:5070")
	refresh.AppendHeader(&sip.ContactHeader{Address: newContact.URI.Clone(), Params: sip.NewParams()})
	merged := refreshInboundSubscriptionDialog(initial, refresh)
	if merged == nil {
		t.Fatal("refreshed dialog snapshot is nil")
	}
	contact, ok := merged.Contact()
	if !ok || contact == nil || contact.Address == nil || contact.Address.Host() != "new-contact.example" {
		t.Fatalf("refreshed Contact = %v", contact)
	}
	recordRoutes := merged.GetHeaders("Record-Route")
	if len(recordRoutes) != 1 {
		t.Fatalf("refreshed Record-Route = %v", recordRoutes)
	}
	route, ok := recordRoutes[0].(*sip.RecordRouteHeader)
	if !ok || len(route.Addresses) != 1 || route.Addresses[0].Host() != "proxy.example" {
		t.Fatalf("refreshed route set = %#v", recordRoutes)
	}
}

func TestTerminatedNotifyClearsOutgoingSubscription(t *testing.T) {
	api := &GB28181API{}
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	api.outgoingSubscriptions.Store("catalog-key", dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-subscription", nil)
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
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
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
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
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
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

func TestTerminatedNotifyRejectsForeignSourceAndDialogTags(t *testing.T) {
	for _, test := range []struct {
		name     string
		deviceID string
		wrongTag bool
	}{
		{name: "foreign source", deviceID: "34020000001320000009"},
		{name: "foreign dialog", deviceID: gb10DeviceID, wrongTag: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			api := &GB28181API{}
			connection := newFlowConnection()
			subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "terminated-protected", []byte("query"))
			dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
			prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
			api.outgoingSubscriptions.Store("protected-key", dialog)
			request := newFlowRequest(t, connection, sip.MethodNotify, "terminated-protected", nil)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			if test.wrongTag {
				from, _ := request.From()
				from.Params.Add("tag", sip.String{Str: "foreign-tag"})
			}
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "terminated;reason=timeout"})
			ctx := &sip.Context{Request: request, Tx: sip.NewTransaction("terminated-protected", connection), DeviceID: test.deviceID, Source: connection.remote}
			api.sipNotifySubscriptionState(ctx)
			if response := <-flowResponse(t, connection); !strings.Contains(response, "SIP/2.0 481") {
				t.Fatalf("foreign terminated NOTIFY response = %s", response)
			}
			if _, exists := api.outgoingSubscriptions.Load("protected-key"); !exists {
				t.Fatal("foreign terminated NOTIFY removed subscription dialog")
			}
		})
	}
}

func prepareOutgoingNotifyDialog(t *testing.T, dialog *outgoingSubscriptionDialog, event, cmdType, targetID string) {
	t.Helper()
	if dialog == nil || dialog.response == nil {
		t.Fatal("subscription dialog response is unavailable")
	}
	callID, ok := dialog.response.CallID()
	if !ok || callID == nil {
		t.Fatal("subscription response missing Call-ID")
	}
	dialog.notify = outgoingSubscriptionNotifyDialog{
		callID:    normalizeCallID(callID),
		localTag:  sipResponseFromTag(dialog.response),
		remoteTag: sipResponseToTag(dialog.response),
		event:     event,
		cmdType:   cmdType,
		deviceID:  gb10DeviceID,
		targetID:  targetID,
		expiresAt: time.Now().Add(time.Minute),
	}
}

func TestActiveNotifyRequiresMatchingOutgoingSubscriptionDialog(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "active-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{response: sip.NewResponseFromRequest("", subscribe, 200, "OK", nil), deviceID: gb10DeviceID}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "Alarm", gb10DeviceID)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("alarm-key", dialog)
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)

	valid := newFlowRequest(t, connection, sip.MethodNotify, "active-subscription", body)
	applyTerminatedNotifyDialog(t, valid, dialog.response)
	valid.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	valid.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, valid, "Alarm", gb10DeviceID); err != nil {
		t.Fatalf("valid active NOTIFY rejected: %v", err)
	}
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, valid, "Alarm", gb10DeviceID); err == nil {
		t.Fatal("replayed NOTIFY CSeq accepted")
	}

	for _, test := range []struct {
		name   string
		mutate func(*sip.Request)
	}{
		{name: "missing Event", mutate: func(request *sip.Request) { request.RemoveHeader("Event") }},
		{name: "missing Subscription-State", mutate: func(request *sip.Request) { request.RemoveHeader("Subscription-State") }},
		{name: "wrong Event", mutate: func(request *sip.Request) {
			request.RemoveHeader("Event")
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog"})
		}},
		{name: "wrong Call-ID", mutate: func(request *sip.Request) {
			request.RemoveHeader("Call-ID")
			callID := sip.CallID("foreign-call-id")
			request.AppendHeader(&callID)
		}},
		{name: "wrong From tag", mutate: func(request *sip.Request) {
			from, _ := request.From()
			from.Params.Add("tag", sip.String{Str: "foreign-tag"})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := newFlowRequest(t, connection, sip.MethodNotify, "active-subscription", body)
			applyTerminatedNotifyDialog(t, request, dialog.response)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
			cseq, _ := request.CSeq()
			cseq.SeqNo = 2
			test.mutate(request)
			if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, request, "Alarm", gb10DeviceID); err == nil {
				t.Fatal("forged active NOTIFY accepted")
			}
		})
	}
}

func TestActiveNotifyMiddlewareRejectsMissingDialog(t *testing.T) {
	connection := newFlowConnection()
	body := []byte(`<Notify><CmdType>Alarm</CmdType><SN>1</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Notify>`)
	request := newFlowRequest(t, connection, sip.MethodNotify, "missing-active-subscription", body)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	api := &GB28181API{}
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("missing-active-subscription", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}
	api.sipNotifySubscriptionState(ctx)
	if response := <-flowResponse(t, connection); !strings.Contains(response, "SIP/2.0 481") {
		t.Fatalf("unsolicited active NOTIFY response = %s", response)
	}
}

func TestPendingNotifyBindsRemoteTagBeforeSubscribeResponse(t *testing.T) {
	connection := newFlowConnection()
	request := newFlowRequest(t, connection, sip.MethodSubscribe, "pending-subscription", []byte("query"))
	dialog := &outgoingSubscriptionDialog{eventValue: "Catalog;id=" + gb10DeviceID}
	dialog.setPendingNotifyDialog(request, "Catalog", gb10DeviceID, gb10DeviceID, 60)
	api := &GB28181API{}
	api.outgoingSubscriptions.Store("pending-key", dialog)

	notify := newFlowRequest(t, connection, sip.MethodNotify, "pending-subscription", []byte(`<Notify><CmdType>Catalog</CmdType><SN>1</SN><DeviceID>`+gb10DeviceID+`</DeviceID></Notify>`))
	notify.RemoveHeader("To")
	local, _ := request.From()
	notify.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	notify.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "pending;expires=60"})
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, notify, "Catalog", gb10DeviceID); err != nil {
		t.Fatalf("early pending NOTIFY rejected: %v", err)
	}

	foreign := newFlowRequest(t, connection, sip.MethodNotify, "pending-subscription", notify.Body())
	foreign.RemoveHeader("To")
	foreign.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
	foreign.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	foreign.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	cseq, _ := foreign.CSeq()
	cseq.SeqNo = 2
	if _, err := api.validateOutgoingSubscriptionNotify(gb10DeviceID, foreign, "Catalog", gb10DeviceID); err == nil {
		t.Fatal("second remote tag replaced early NOTIFY dialog binding")
	}
}

func applyTerminatedNotifyDialog(t *testing.T, request *sip.Request, response *sip.Response) {
	t.Helper()
	request.RemoveHeader("From")
	request.RemoveHeader("To")
	remote, ok := response.To()
	if !ok || remote == nil {
		t.Fatal("subscription response missing remote address")
	}
	local, ok := response.From()
	if !ok || local == nil {
		t.Fatal("subscription response missing local address")
	}
	request.AppendHeader(&sip.FromHeader{Address: remote.Address.Clone(), Params: remote.Params.Clone()})
	request.AppendHeader(&sip.ToHeader{Address: local.Address.Clone(), Params: local.Params.Clone()})
}

func applyInboundSubscribeDialog(t *testing.T, request *sip.Request, localTag string, cseq uint32) {
	t.Helper()
	to, ok := request.To()
	if !ok || to == nil || to.Address == nil {
		t.Fatal("SUBSCRIBE request missing To header")
	}
	params := sip.NewParams()
	if to.Params != nil {
		params = to.Params.Clone()
	}
	if strings.TrimSpace(localTag) != "" {
		params.Add("tag", sip.String{Str: localTag})
	}
	request.RemoveHeader("To")
	request.AppendHeader(&sip.ToHeader{DisplayName: to.DisplayName, Address: to.Address.Clone(), Params: params})
	current, ok := request.CSeq()
	if !ok || current == nil {
		t.Fatal("SUBSCRIBE request missing CSeq")
	}
	current.SeqNo = cseq
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
	applyInboundSubscribeDialog(t, req, firstSubscription.LocalTag, 2)
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
	applyInboundSubscribeDialog(t, req, firstSubscription.LocalTag, 3)
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

func TestInboundSubscribeRejectsInvalidDialogWithoutSideEffects(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>58</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "subscribe-dialog-security"

	initial := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
	initialFrom, _ := initial.From()
	initial.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
	initial.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "60"})
	ctx := &sip.Context{
		Request: initial, Tx: sip.NewTransaction("subscribe-dialog-security-initial", conn), DeviceID: gb10PlatformID,
		Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
	}
	api.sipSubscribeEvent(ctx)
	assertFlowOK(t, <-flowResponse(t, conn))

	var subscription *eventSubscription
	api.eventSubscribers.Range(func(_, value any) bool {
		subscription, _ = value.(*eventSubscription)
		return false
	})
	if subscription == nil || subscription.LocalTag == "" {
		t.Fatal("initial subscription dialog was not stored")
	}
	subscription.mu.Lock()
	subscription.Filter = eventSubscriptionFilter{AlarmMethod: "5"}
	subscription.DownstreamKeys = []string{"protected-downstream"}
	wantExpiry := subscription.ExpiresAt
	wantFilter := subscription.Filter
	wantDownstream := append([]string(nil), subscription.DownstreamKeys...)
	wantCSeq := subscription.RemoteCSeq
	localTag := subscription.LocalTag
	subscription.mu.Unlock()

	tests := []struct {
		name    string
		request string
		expires string
		toTag   string
		cseq    uint32
	}{
		{name: "create with To tag", request: callID + "-new", expires: "60", toTag: localTag, cseq: 1},
		{name: "refresh without To tag", request: callID, expires: "120", cseq: 2},
		{name: "refresh with wrong To tag", request: callID, expires: "120", toTag: "wrong-local-tag", cseq: 2},
		{name: "refresh replays CSeq", request: callID, expires: "120", toTag: localTag, cseq: 1},
		{name: "cancel without To tag", request: callID, expires: "0", cseq: 2},
		{name: "cancel with wrong To tag", request: callID, expires: "0", toTag: "wrong-local-tag", cseq: 2},
		{name: "cancel replays CSeq", request: callID, expires: "0", toTag: localTag, cseq: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := newFlowRequest(t, conn, sip.MethodSubscribe, test.request, body)
			request.RemoveHeader("From")
			request.AppendHeader(initialFrom.Clone())
			applyInboundSubscribeDialog(t, request, test.toTag, test.cseq)
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
			request.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: test.expires})
			ctx.Request = request
			ctx.Tx = sip.NewTransaction("subscribe-dialog-security-"+test.name, conn)
			api.sipSubscribeEvent(ctx)
			if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 481") {
				t.Fatalf("invalid SUBSCRIBE response = %s", response)
			}

			value, loaded := api.eventSubscribers.Load(subscription.Key)
			if !loaded || value != subscription {
				t.Fatal("invalid SUBSCRIBE replaced or removed the existing dialog")
			}
			subscription.mu.Lock()
			gotExpiry := subscription.ExpiresAt
			gotFilter := subscription.Filter
			gotDownstream := append([]string(nil), subscription.DownstreamKeys...)
			gotCSeq := subscription.RemoteCSeq
			subscription.mu.Unlock()
			if !gotExpiry.Equal(wantExpiry) || gotFilter != wantFilter || !slices.Equal(gotDownstream, wantDownstream) || gotCSeq != wantCSeq {
				t.Fatalf("invalid SUBSCRIBE changed state: expiry=%v filter=%+v downstream=%v CSeq=%d", gotExpiry, gotFilter, gotDownstream, gotCSeq)
			}
		})
	}
}

func TestCatalogSubscriptionDialogIsolatedBySubscriber(t *testing.T) {
	api := &GB28181API{}
	conn := newFlowConnection()
	body := []byte(`<?xml version="1.0"?><Query><CmdType>Catalog</CmdType><SN>59</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
	const callID = "shared-subscribe-dialog"
	const fromTag = "shared-from-tag"
	makeContext := func(subscriberID, expires, txID string) *sip.Context {
		req := newFlowRequest(t, conn, sip.MethodSubscribe, callID, body)
		req.RemoveHeader("From")
		from := mustFlowAddress(t, "sip:"+subscriberID+"@3402000000")
		from.Params.Add("tag", sip.String{Str: fromTag})
		req.AppendHeader(&sip.FromHeader{Address: from.URI, Params: from.Params})
		req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "Catalog;id=" + gb10DeviceID})
		req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: expires})
		return &sip.Context{
			Request: req, Tx: sip.NewTransaction(txID, conn), DeviceID: subscriberID,
			Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(GBVersion11),
		}
	}

	api.sipSubscribeEvent(makeContext(gb10PlatformID, "60", "subscriber-a"))
	assertFlowOK(t, <-flowResponse(t, conn))
	api.sipSubscribeEvent(makeContext("44010000002000000001", "0", "subscriber-b-cancel"))
	if response := <-flowResponse(t, conn); !strings.Contains(response, "SIP/2.0 481") {
		t.Fatalf("different subscriber cancel response = %s", response)
	}

	wantKey := buildEventSubscriptionKey("device:"+gb10PlatformID, callID, fromTag, "Catalog", gb10DeviceID)
	if _, ok := api.eventSubscribers.Load(wantKey); !ok {
		t.Fatal("different subscriber cancelled the existing dialog")
	}
}

func TestEventSubscriptionKeyIsolatesCascadeWorkers(t *testing.T) {
	first := newCascadeWorker(nil, testSharedCascadePlatform(t))
	secondPlatform := testSharedCascadePlatform(t)
	secondPlatform.name = "secondary"
	second := newCascadeWorker(nil, secondPlatform)
	ctx := &sip.Context{DeviceID: first.platform.serverID}

	firstKey := buildEventSubscriptionKey(subscriptionOwnerKey(ctx, first), "shared-call", "shared-tag", "Catalog", first.platform.localID)
	secondKey := buildEventSubscriptionKey(subscriptionOwnerKey(ctx, second), "shared-call", "shared-tag", "Catalog", first.platform.localID)
	if firstKey == secondKey {
		t.Fatalf("different cascade workers share subscription key %q", firstKey)
	}
	if buildEventSubscriptionKey("device:a", "b|c", "d", "Catalog", "e") ==
		buildEventSubscriptionKey("device:a", "b", "c|d", "Catalog", "e") {
		t.Fatal("subscription key is ambiguous when SIP dialog fields contain separators")
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

func TestAlarmSubscriptionBusinessResponseVersionMatrix(t *testing.T) {
	tests := []struct {
		version GBProtocolVersion
		wantXML bool
	}{
		{version: GBVersion10, wantXML: true},
		{version: GBVersion11, wantXML: true},
		{version: GBVersion20, wantXML: true},
		{version: GBVersion30, wantXML: false},
	}
	for _, test := range tests {
		t.Run(string(test.version), func(t *testing.T) {
			api := &GB28181API{}
			conn := newFlowConnection()
			body := []byte(`<?xml version="1.0"?><Query><CmdType>Alarm</CmdType><SN>52</SN><DeviceID>` + gb10DeviceID + `</DeviceID></Query>`)
			req := newFlowRequest(t, conn, sip.MethodSubscribe, "subscribe-alarm-"+string(test.version), body)
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
			req.AppendHeader(&sip.GenericHeader{HeaderName: "Expires", Contents: "90"})
			ctx := &sip.Context{
				Request: req, Tx: sip.NewTransaction("subscribe-alarm-tx-"+string(test.version), conn), DeviceID: gb10PlatformID,
				Source: conn.remote, To: mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000"), XGBVer: string(test.version),
			}
			api.sipSubscribeEvent(ctx)
			response := <-flowResponse(t, conn)
			hasXML := strings.Contains(response, "<Response>") && strings.Contains(response, "<CmdType>Alarm</CmdType>") &&
				strings.Contains(response, "<SN>52</SN>") && strings.Contains(response, "<Result>OK</Result>")
			if hasXML != test.wantXML {
				t.Fatalf("%s Alarm SUBSCRIBE XML response = %v, want %v:\n%s", test.version, hasXML, test.wantXML, response)
			}
		})
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
