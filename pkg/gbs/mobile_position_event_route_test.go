package gbs

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

const secondMobilePositionChannelID = "34020000001320000003"

func mobilePositionBatchForRouteTest() []byte {
	return []byte(`<Notify><CmdType>MobilePosition</CmdType><SN>92</SN><DeviceID>` + gb10DeviceID +
		`</DeviceID><Time>2026-09-02T10:00:01</Time><SumNum>2</SumNum><DeviceList Num="2">` +
		`<Item><DeviceID>` + gb10ChannelID + `</DeviceID><CaptureTime>2026-09-02T10:00:00</CaptureTime>` +
		`<Longitude>120.1</Longitude><Latitude>30.1</Latitude><Height>5</Height></Item>` +
		`<Item><DeviceID>` + secondMobilePositionChannelID + `</DeviceID><CaptureTime>2026-09-02T10:00:00</CaptureTime>` +
		`<Longitude>121.2</Longitude><Latitude>31.2</Latitude><Height>6</Height></Item>` +
		`</DeviceList></Notify>`)
}

func TestLocalMobilePositionSubscriptionFiltersBatchByTargetAndVersion(t *testing.T) {
	body := mobilePositionBatchForRouteTest()
	targets := []string{gb10ChannelID, secondMobilePositionChannelID}

	tests := []struct {
		name        string
		version     GBProtocolVersion
		targetID    string
		wantCount   int
		want        []string
		forbidden   []string
		wantBatches int
	}{
		{
			name: "2022 device subscription keeps one aggregate batch", version: GBVersion30, targetID: gb10DeviceID,
			wantCount: 1, wantBatches: 1, want: []string{"<SumNum>2</SumNum>", gb10ChannelID, secondMobilePositionChannelID},
		},
		{
			name: "2022 first channel subscription receives only its item", version: GBVersion30, targetID: gb10ChannelID,
			wantCount: 1, wantBatches: 1, want: []string{"<SumNum>1</SumNum>", gb10ChannelID}, forbidden: []string{secondMobilePositionChannelID},
		},
		{
			name: "2022 second channel subscription is not lost", version: GBVersion30, targetID: secondMobilePositionChannelID,
			wantCount: 1, wantBatches: 1, want: []string{"<SumNum>1</SumNum>", secondMobilePositionChannelID}, forbidden: []string{gb10ChannelID},
		},
		{
			name: "2016 device subscription receives one notify per item", version: GBVersion20, targetID: gb10DeviceID,
			wantCount: 2, want: []string{"<Longitude>120.1</Longitude>", "<Longitude>121.2</Longitude>"},
			forbidden: []string{"<SumNum>", "<DeviceList", "<Height>"},
		},
		{
			name: "unrelated channel subscription is filtered", version: GBVersion30, targetID: "34020000001320000099",
			wantCount: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payloads, err := rewriteLocalMobilePositionForSubscription(body, gb10DeviceID, targets, test.version, test.targetID)
			if err != nil {
				t.Fatal(err)
			}
			if len(payloads) != test.wantCount {
				t.Fatalf("payload count = %d, want %d", len(payloads), test.wantCount)
			}
			combined := ""
			for _, payload := range payloads {
				combined += string(payload)
			}
			if got := strings.Count(combined, "<DeviceList"); got != test.wantBatches {
				t.Fatalf("DeviceList count = %d, want %d: %s", got, test.wantBatches, combined)
			}
			for _, expected := range test.want {
				if !strings.Contains(combined, expected) {
					t.Fatalf("payload does not contain %q: %s", expected, combined)
				}
			}
			for _, forbidden := range test.forbidden {
				if strings.Contains(combined, forbidden) {
					t.Fatalf("payload contains %q: %s", forbidden, combined)
				}
			}
		})
	}
}

func TestPublishBatchMobilePositionRoutesEveryLocalTargetOnce(t *testing.T) {
	platform := mustFlowAddress(t, "sip:"+gb10PlatformID+"@3402000000")
	sipServer := sip.NewServer(platform)
	server := &Server{Server: sipServer, fromAddress: *platform}
	api := &GB28181API{svr: server}
	server.gb = api
	t.Cleanup(server.Close)

	addSubscription := func(key, targetID string) *flowConnection {
		base := newFlowConnection()
		conn := &tcpFlowConnection{flowConnection: base}
		sub := &eventSubscription{
			Key: key, CmdType: "MobilePosition", DeviceID: targetID, Event: "presence",
			ExpiresAt: time.Now().Add(time.Minute), To: mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
			Source: base.remote, Conn: conn, GBVersion: string(GBVersion30),
		}
		attachFlowEventSubscriptionDialog(t, sub, base, key+"-dialog")
		api.eventSubscribers.Store(key, sub)
		return base
	}

	deviceConn := addSubscription("mobile-device", gb10DeviceID)
	firstConn := addSubscription("mobile-first", gb10ChannelID)
	secondConn := addSubscription("mobile-second", secondMobilePositionChannelID)
	unrelatedConn := addSubscription("mobile-unrelated", "34020000001320000099")

	api.publishEventNotifyForTargets("MobilePosition", gb10DeviceID,
		[]string{gb10ChannelID, secondMobilePositionChannelID}, mobilePositionBatchForRouteTest())

	assertNotify := func(name string, connection *flowConnection, contains []string, forbidden string) {
		t.Helper()
		select {
		case payload := <-connection.writes:
			text := string(payload)
			if !strings.HasPrefix(text, "NOTIFY ") {
				t.Fatalf("%s payload is not NOTIFY: %s", name, text)
			}
			for _, expected := range contains {
				if !strings.Contains(text, expected) {
					t.Fatalf("%s NOTIFY does not contain %q: %s", name, expected, text)
				}
			}
			if forbidden != "" && strings.Contains(text, forbidden) {
				t.Fatalf("%s NOTIFY contains unrelated target %q: %s", name, forbidden, text)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s MobilePosition NOTIFY was not sent", name)
		}
	}

	assertNotify("device", deviceConn, []string{"<SumNum>2</SumNum>", gb10ChannelID, secondMobilePositionChannelID}, "")
	assertNotify("first channel", firstConn, []string{"<SumNum>1</SumNum>", gb10ChannelID}, secondMobilePositionChannelID)
	assertNotify("second channel", secondConn, []string{"<SumNum>1</SumNum>", secondMobilePositionChannelID}, gb10ChannelID)
	select {
	case payload := <-unrelatedConn.writes:
		t.Fatalf("unrelated subscription received MobilePosition NOTIFY: %s", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEventSubscriptionMatchesAnyBatchTarget(t *testing.T) {
	targets := []string{gb10ChannelID, secondMobilePositionChannelID}
	for _, target := range []string{"", "*", gb10DeviceID, gb10ChannelID, secondMobilePositionChannelID} {
		if !eventSubscriptionMatchesTargets(target, gb10DeviceID, targets) {
			t.Fatalf("subscription target %q did not match batch", target)
		}
	}
	if eventSubscriptionMatchesTargets("34020000001320000099", gb10DeviceID, targets) {
		t.Fatal("unrelated subscription target matched batch")
	}
}

func TestBatchMobilePositionNotifyMatchesChannelSubscriptionDialog(t *testing.T) {
	connection := newFlowConnection()
	subscribe := newFlowRequest(t, connection, sip.MethodSubscribe, "batch-mobile-position-dialog", []byte("query"))
	dialog := &outgoingSubscriptionDialog{
		response: sip.NewResponseFromRequest("", subscribe, http.StatusOK, "OK", nil),
		deviceID: gb10DeviceID,
	}
	prepareOutgoingNotifyDialog(t, dialog, "presence", "MobilePosition", secondMobilePositionChannelID)
	api := &GB28181API{}
	key := "batch-mobile-position-key"
	api.outgoingSubscriptions.Store(key, dialog)

	request := newFlowRequest(t, connection, sip.MethodNotify, "batch-mobile-position-dialog", mobilePositionBatchForRouteTest())
	applyTerminatedNotifyDialog(t, request, dialog.response)
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Event", Contents: "presence"})
	request.AppendHeader(&sip.GenericHeader{HeaderName: "Subscription-State", Contents: "active;expires=60"})
	ctx := &sip.Context{
		Request: request, Tx: sip.NewTransaction("batch-mobile-position-dialog", connection),
		DeviceID: gb10DeviceID, Source: connection.remote,
	}

	api.sipNotifySubscriptionState(ctx)
	matched, ok := ctx.Get(outgoingSubscriptionNotifyContextKey)
	if !ok || matched != key {
		t.Fatalf("matched batch MobilePosition subscription key = %#v, %v", matched, ok)
	}
	value, ok := ctx.Get(outgoingSubscriptionCommitContextKey)
	commit, ok := value.(*outgoingSubscriptionNotifyCommit)
	if !ok || commit == nil || len(commit.targetIDs) != 3 {
		t.Fatalf("batch MobilePosition commit targets = %#v", value)
	}
	if err := ctx.RespondString(http.StatusOK, "OK"); err != nil {
		t.Fatal(err)
	}
	if !api.commitOutgoingSubscriptionNotifyAfterResponse(ctx) {
		t.Fatal("batch MobilePosition subscription dialog was not committed")
	}
	requestCSeq, _ := request.CSeq()
	if got := dialog.snapshotNotifyDialog().cseq; got != requestCSeq.SeqNo {
		t.Fatalf("committed NOTIFY CSeq = %d, want %d", got, requestCSeq.SeqNo)
	}
}
