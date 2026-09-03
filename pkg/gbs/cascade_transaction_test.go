package gbs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestCascadeExchangeCompletionClosesOnlyNonInviteTransaction(t *testing.T) {
	target, err := sip.ParseSipURI("sip:" + gb10PlatformID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	worker := &cascadeWorker{}

	message := sip.NewRequest("", sip.MethodMessage, &target, sip.DefaultSipVersion, nil, nil)
	messageTX := sip.NewTransaction("completed-cascade-message", nil)
	worker.completeExchangeTransaction(message, nil, messageTX)
	if response, err := messageTX.GetResponseContext(t.Context()); response != nil || err != nil {
		t.Fatalf("completed cascade MESSAGE transaction remained active: response=%v err=%v", response, err)
	}

	invite := sip.NewRequest("", sip.MethodInvite, &target, sip.DefaultSipVersion, nil, nil)
	inviteTX := sip.NewTransaction("completed-cascade-invite", nil)
	defer inviteTX.Close()
	worker.completeExchangeTransaction(invite, nil, inviteTX)
	waitCtx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if response, err := inviteTX.GetResponseContext(waitCtx); response != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completed cascade INVITE transaction was closed before ACK window: response=%v err=%v", response, err)
	}
}
