package sms

import (
	"context"
	"testing"

	"github.com/gowvp/owl/pkg/zlm"
)

func TestLalmaxUnsupportedRTPOperationsReturnErrors(t *testing.T) {
	driver := NewLalmaxDriver()
	server := &MediaServer{}
	if _, err := driver.CloseRTPServer(context.Background(), server, &zlm.CloseRTPServerRequest{}); err == nil {
		t.Fatal("CloseRTPServer error = nil")
	}
	if _, err := driver.StartSendRTP(context.Background(), server, &zlm.StartSendRTPRequest{}); err == nil {
		t.Fatal("StartSendRTP error = nil")
	}
	if _, err := driver.StartSendRTPTalk(context.Background(), server, &zlm.StartSendRTPTalkRequest{}); err == nil {
		t.Fatal("StartSendRTPTalk error = nil")
	}
	if _, err := driver.StopSendRTP(context.Background(), server, &zlm.StopSendRTPRequest{}); err == nil {
		t.Fatal("StopSendRTP error = nil")
	}
}
