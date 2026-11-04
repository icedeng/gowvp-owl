package api

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/sms"
	"github.com/gowvp/owl/internal/core/sms/store/smsdb"
	"gorm.io/gorm"
)

func TestMediaInspectionRoutesReadOnlyAndNodeScoped(t *testing.T) {
	queries := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries++
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["secret"] != "private-node-secret" {
			t.Errorf("unexpected node credentials: %v", body)
		}
		if r.URL.Path == "/index/api/getMediaList" {
			fmt.Fprint(w, `{"code":0,"data":[{"stream":"node-stream","originUrl":"rtsp://u:p@192.0.2.1/x?token=private"}]}`)
		} else if r.URL.Path == "/index/api/version" {
			http.NotFound(w, r)
		} else if r.URL.Path == "/index/api/getAllSession" {
			fmt.Fprint(w, `{"code":-100,"msg":"private-node-secret"}`)
		} else {
			t.Errorf("inspection invoked unexpected endpoint: %s", r.URL.Path)
			w.WriteHeader(500)
		}
	}))
	defer upstream.Close()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name())))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	store := smsdb.NewDB(db).AutoMigrate(true)
	host, port, err := net.SplitHostPort(upstream.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []*sms.MediaServer{
		{ID: "zlm-test", IP: host, Ports: sms.MediaServerPorts{HTTP: portNumber}, Type: "zlm", Secret: "private-node-secret"},
		{ID: "lal-test", Type: "lalmax"},
	} {
		if err := store.MediaServer().Create(t.Context(), node); err != nil {
			t.Fatal(err)
		}
	}
	core := sms.NewCore(store)
	defer core.Close()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	registerSms(router, NewSmsAPI(core), func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer test" {
			c.AbortWithStatus(401)
		}
	})
	request := func(method, path string, auth bool) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer test")
		}
		router.ServeHTTP(response, req)
		return response
	}
	before := time.Now()
	response := request("GET", "/media_servers/zlm-test/inspect/streams", true)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "node-stream") || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "u:p") {
		t.Fatalf("unexpected query response: %d %s", response.Code, response.Body.String())
	}
	var result sms.InspectionResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || result.Status != "ok" || result.FetchedAt.Before(before) {
		t.Fatalf("invalid query envelope: %v %+v", err, result)
	}
	for path, want := range map[string]string{
		"/media_servers/zlm-test/inspect/version":  "unsupported",
		"/media_servers/zlm-test/inspect/sessions": "unavailable",
		"/media_servers/lal-test/inspect/streams":  "unsupported",
	} {
		response := request("GET", path, true)
		if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status != want {
			t.Errorf("%s: got %d %s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/media_servers/zlm-test", "/media_servers/zlm-test/inspect/streams"} {
		if response := request("GET", path, false); response.Code != 401 {
			t.Errorf("unprotected query route %s: %d", path, response.Code)
		}
	}
	for _, path := range []string{"/media_servers/zlm-test/inspect/setServerConfig", "/media_servers/zlm-test/inspect/restartServer", "/media_servers/zlm-test/inspect/stream_info", "/media_servers/zlm-test/inspect/rtp_info", "/media_servers/missing/inspect/streams"} {
		if response := request("GET", path, true); response.Code < 400 {
			t.Errorf("invalid query accepted %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	if response := request("POST", "/media_servers/zlm-test/inspect/streams", true); response.Code != 404 {
		t.Errorf("unexpected non-GET inspection route: %d", response.Code)
	}
	response = request("GET", "/media_servers/zlm-test", true)
	if response.Code != 200 || strings.Contains(response.Body.String(), "secret") {
		t.Fatalf("detail leaked secret: %s", response.Body.String())
	}
	response = request("GET", "/media_servers", true)
	if response.Code != 200 || strings.Contains(response.Body.String(), "secret") || !strings.Contains(response.Body.String(), "zlm-test") {
		t.Fatalf("invalid or credential-bearing node list: %s", response.Body.String())
	}
	if queries != 3 {
		t.Fatalf("unexpected upstream calls, read performed writes or invalid inputs reached node: %d", queries)
	}
}
