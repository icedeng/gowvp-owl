package onvifserver

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gowvp/onvif/server"
	"github.com/gowvp/owl/internal/conf"
)

type credentialTestProvider struct{}

func (credentialTestProvider) ListStreamProfiles() []server.StreamProfile { return nil }
func (credentialTestProvider) GetStreamURI(string, string) string         { return "" }
func (credentialTestProvider) GetSnapshotURI(string, string) string       { return "" }

func TestSOAPUsesIndependentCredentialsAndFailsClosed(t *testing.T) {
	cfg := conf.DefaultConfig()
	cfg.Server.Username, cfg.Server.Password = "admin", "admin-password"
	cfg.Server.ONVIF = conf.ServerONVIF{Username: "onvif-client", Password: "onvif-password"}
	body := func(user, password string) string {
		return fmt.Sprintf(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Header><wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"><wsse:UsernameToken><wsse:Username>%s</wsse:Username><wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText">%s</wsse:Password></wsse:UsernameToken></wsse:Security></s:Header><s:Body><tds:GetDeviceInformation xmlns:tds="http://www.onvif.org/ver10/device/wsdl"/></s:Body></s:Envelope>`, user, password)
	}
	for _, tc := range []struct {
		user, password string
		want           int
	}{{"admin", "admin-password", 401}, {"onvif-client", "onvif-password", 200}, {"onvif-client", "wrong", 401}} {
		w := httptest.NewRecorder()
		soapHandler(credentialTestProvider{}, &cfg, "test", "127.0.0.1").ServeHTTP(w, httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body(tc.user, tc.password))))
		if w.Code != tc.want {
			t.Fatalf("user=%s status=%d want=%d body=%s", tc.user, w.Code, tc.want, w.Body.String())
		}
	}
	cfg.Server.ONVIF = conf.ServerONVIF{}
	w := httptest.NewRecorder()
	soapHandler(nil, &cfg, "test", "").ServeHTTP(w, httptest.NewRequest("POST", "/onvif/device_service", strings.NewReader(body("admin", "admin-password"))))
	if w.Code != 401 {
		t.Fatal("empty ONVIF credentials allowed anonymous access")
	}
}
