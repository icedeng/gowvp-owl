package gbs

import (
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	"github.com/gowvp/owl/internal/core/sms"
)

func TestCascadeMediaServerIDUsesChannelBinding(t *testing.T) {
	tests := []struct {
		name    string
		channel *ipc.Channel
		want    string
	}{
		{name: "nil channel", want: sms.DefaultMediaServerID},
		{name: "empty binding", channel: &ipc.Channel{}, want: sms.DefaultMediaServerID},
		{name: "trimmed binding", channel: &ipc.Channel{Config: ipc.StreamConfig{MediaServerID: " edge-zlm-4 "}}, want: "edge-zlm-4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := cascadeMediaServerID(test.channel); got != test.want {
				t.Fatalf("cascadeMediaServerID() = %q, want %q", got, test.want)
			}
		})
	}
}
