package gbs

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestInboundOptionsAdvertisesProductionMethodsForAllVersions(t *testing.T) {
	want := map[string]struct{}{
		sip.MethodInvite:    {},
		sip.MethodACK:       {},
		sip.MethodCancel:    {},
		sip.MethodOptions:   {},
		sip.MethodBYE:       {},
		sip.MethodMessage:   {},
		sip.MethodRegister:  {},
		sip.MethodSubscribe: {},
		sip.MethodNotify:    {},
		sip.MethodInfo:      {},
	}

	for _, version := range []GBProtocolVersion{GBVersion10, GBVersion11, GBVersion20, GBVersion30} {
		t.Run(string(version), func(t *testing.T) {
			conn := newFlowConnection()
			request := newFlowRequest(t, conn, sip.MethodOptions, "options-"+string(version), nil)
			request.RemoveHeader("X-GB-Ver")
			xgbVersion := sip.XGBVer(version)
			request.AppendHeader(&xgbVersion)
			tx := sip.NewTransaction("options-"+string(version)+"-tx", conn)
			api := &GB28181API{}
			api.sipOptionsGeneric(&sip.Context{
				Request:   request,
				Tx:        tx,
				DeviceID:  gb10DeviceID,
				Source:    conn.remote,
				To:        mustFlowAddress(t, "sip:"+gb10DeviceID+"@3402000000"),
				Log:       slog.Default(),
				XGBVer:    string(version),
				XGBVerRaw: string(version),
			})

			var payload string
			select {
			case response := <-conn.writes:
				payload = string(response)
			case <-time.After(time.Second):
				t.Fatal("OPTIONS response timeout")
			}
			if !strings.HasPrefix(payload, "SIP/2.0 200 OK\r\n") {
				t.Fatalf("unexpected OPTIONS response:\n%s", payload)
			}
			got := parseAllowMethods(t, payload)
			if len(got) != len(want) {
				t.Fatalf("Allow methods = %#v, want %#v", got, want)
			}
			for method := range want {
				if _, ok := got[method]; !ok {
					t.Errorf("Allow header missing %s: %#v", method, got)
				}
			}
		})
	}
}

func parseAllowMethods(t *testing.T, response string) map[string]struct{} {
	t.Helper()
	for _, line := range strings.Split(response, "\r\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Allow") {
			continue
		}
		methods := make(map[string]struct{})
		for _, method := range strings.Split(value, ",") {
			method = strings.TrimSpace(method)
			if method == "" {
				t.Fatalf("Allow header contains an empty method: %q", line)
			}
			if _, exists := methods[method]; exists {
				t.Fatalf("Allow header contains duplicate method %q", method)
			}
			methods[method] = struct{}{}
		}
		return methods
	}
	t.Fatal("OPTIONS response has no Allow header")
	return nil
}
