package gbs

import (
	"net/http"
	"strings"
	"testing"

	"github.com/gowvp/owl/pkg/gbs/annexg"
	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestStreamCSeqExhaustionDoesNotWrap(t *testing.T) {
	stream := &Streams{CseqNo: sip.MaxCSeq - 1}
	if got, err := stream.nextCSeq(); err != nil || got != sip.MaxCSeq {
		t.Fatalf("last valid stream CSeq = %d, %v", got, err)
	}
	if got, err := stream.nextCSeq(); err == nil || got != 0 || stream.CseqNo != sip.MaxCSeq {
		t.Fatalf("exhausted stream CSeq = %d, state=%d, err=%v", got, stream.CseqNo, err)
	}
}

func TestEventSubscriptionCSeqExhaustionDoesNotWrap(t *testing.T) {
	sub := &eventSubscription{CSeq: sip.MaxCSeq}
	sub.mu.Lock()
	got, err := reserveEventSubscriptionCSeqLocked(sub)
	sub.mu.Unlock()
	if err == nil || got != 0 || sub.CSeq != sip.MaxCSeq {
		t.Fatalf("exhausted subscription CSeq = %d, state=%d, err=%v", got, sub.CSeq, err)
	}
}

func TestInboundDialogCSeqExhaustionDoesNotWrap(t *testing.T) {
	dialog := &inboundInviteDialog{LocalCSeq: sip.MaxCSeq}
	dialog.mu.Lock()
	got, err := reserveLocalCSeqLocked(dialog)
	dialog.mu.Unlock()
	if err == nil || got != 0 || dialog.LocalCSeq != sip.MaxCSeq {
		t.Fatalf("exhausted dialog CSeq = %d, state=%d, err=%v", got, dialog.LocalCSeq, err)
	}
}

func TestCascadeRegisterStartsNewSequenceAfterCSeqExhaustion(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	worker.mu.Lock()
	worker.cseq = sip.MaxCSeq
	previousCallID := worker.callID
	worker.mu.Unlock()
	request := worker.newRegisterRequest(3600, nil)
	cseq, ok := request.CSeq()
	callID, callIDOK := request.CallID()
	if !ok || cseq == nil || cseq.SeqNo != 1 || !callIDOK || callID == nil || *callID == previousCallID {
		t.Fatalf("rotated REGISTER sequence = CSeq:%v Call-ID:%v previous:%v", cseq, callID, previousCallID)
	}
}

func TestCascadeDigestRetryRejectsExhaustedCSeq(t *testing.T) {
	worker := newCascadeWorker(nil, testCascadePlatform(t, "3.0"))
	request := worker.newRegisterRequest(3600, nil)
	cseq, _ := request.CSeq()
	cseq.SeqNo = sip.MaxCSeq
	auth := sip.AuthFromValue(`Digest realm="3401000000",nonce="nonce",algorithm=MD5`)
	if retry, err := buildCascadeRequestDigestRetry(request, "Authorization", auth); err == nil || retry != nil {
		t.Fatalf("exhausted cascade Digest CSeq accepted: request=%v err=%v", retry, err)
	}
}

func TestAnnexGDigestRetryRejectsExhaustedCSeq(t *testing.T) {
	recipient, err := sip.ParseURI("sip:" + gb10PlatformID + "@192.0.2.30:5060")
	if err != nil {
		t.Fatal(err)
	}
	local, err := sip.ParseURI("sip:" + gb10DeviceID + "@192.0.2.20:5060")
	if err != nil {
		t.Fatal(err)
	}
	callID := sip.CallID("annex-g-cseq-limit")
	request := sip.NewRequest("", sip.MethodMessage, recipient, sip.DefaultSipVersion,
		sip.NewHeaderBuilder().SetMethod(sip.MethodMessage).SetSeqNo(uint(sip.MaxCSeq)).
			SetFrom(&sip.Address{URI: local, Params: sip.NewParams()}).
			SetTo(&sip.Address{URI: recipient, Params: sip.NewParams()}).SetCallID(&callID).
			AddVia(&sip.ViaHop{Params: sip.NewParams().Add("branch", sip.String{Str: sip.GenerateBranch()})}).Build(), nil)
	challenge := sip.NewResponseFromRequest("", request, http.StatusUnauthorized, "Unauthorized", nil)
	challenge.AppendHeader(&sip.GenericHeader{HeaderName: "WWW-Authenticate", Contents: `Digest realm="3401000000",nonce="nonce",algorithm=MD5`})
	system := &annexGSystem{id: gb10DeviceID, realm: "3401000000", password: "secret", version: annexg.Version2016}
	if retry, err := buildAnnexGDigestRetry(request, challenge, system, system.realm); err == nil || retry != nil || !strings.Contains(err.Error(), "CSeq") {
		t.Fatalf("exhausted Annex G Digest CSeq accepted: request=%v err=%v", retry, err)
	}
}
