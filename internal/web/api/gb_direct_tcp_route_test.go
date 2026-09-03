package api

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterGB28181DirectTCPDownloadRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	registerGB28181(engine, IPCAPI{})
	want := map[string]bool{
		"GET /gb28181/downloads/:session_id":                                  false,
		"DELETE /gb28181/downloads/:session_id":                               false,
		"GET /gb28181/metrics":                                                false,
		"GET /gb28181/cascade/status":                                         false,
		"GET /gb28181/annex-g/alarms":                                         false,
		"GET /gb28181/annex-g/defences":                                       false,
		"GET /gb28181/annex-g/defence-audits":                                 false,
		"GET /devices/:id/subscriptions":                                      false,
		"GET /channels/:id/history/status":                                    false,
		"PUT /channels/:id/media_server":                                      false,
		"GET /gb28181/devices/:device_id/channels/:channel_id/history/status": false,
		"GET /channels/gb28181/:device_id/:channel_id/history/status":         false,
	}
	for _, route := range engine.Routes() {
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, found := range want {
		if !found {
			t.Fatalf("route %s was not registered", route)
		}
	}
}
