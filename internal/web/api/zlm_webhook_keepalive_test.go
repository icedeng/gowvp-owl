package api

import "testing"

type recordingMediaServerHeartbeater struct {
	ids []string
}

func (heartbeater *recordingMediaServerHeartbeater) Keepalive(serverID string) {
	heartbeater.ids = append(heartbeater.ids, serverID)
}

func TestServerKeepaliveUsesReportedMediaServerID(t *testing.T) {
	heartbeater := new(recordingMediaServerHeartbeater)
	webhook := WebHookAPI{smsCore: heartbeater}

	output, err := webhook.onServerKeepalive(nil, &onServerKeepaliveInput{MediaServerID: " edge-zlm-1 "})
	if err != nil {
		t.Fatal(err)
	}
	if output.Code != 0 {
		t.Fatalf("keepalive output = %+v", output)
	}
	if len(heartbeater.ids) != 1 || heartbeater.ids[0] != "edge-zlm-1" {
		t.Fatalf("keepalive server IDs = %v", heartbeater.ids)
	}
}

func TestServerKeepaliveFallsBackToDefaultMediaServer(t *testing.T) {
	heartbeater := new(recordingMediaServerHeartbeater)
	webhook := WebHookAPI{smsCore: heartbeater}

	if _, err := webhook.onServerKeepalive(nil, &onServerKeepaliveInput{}); err != nil {
		t.Fatal(err)
	}
	if len(heartbeater.ids) != 1 || heartbeater.ids[0] != "local" {
		t.Fatalf("keepalive server IDs = %v", heartbeater.ids)
	}
}
