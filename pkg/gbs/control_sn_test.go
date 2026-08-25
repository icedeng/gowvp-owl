package gbs

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestProtocolSNRemainsPositiveAndWrapsAtMaxInt32(t *testing.T) {
	api := &GB28181API{}
	api.querySN.Store(math.MaxInt32 - 1)
	if got := api.nextQuerySN(); got != math.MaxInt32 {
		t.Fatalf("query SN before wrap = %d", got)
	}
	if got := api.nextQuerySN(); got != 1 {
		t.Fatalf("query SN after wrap = %d", got)
	}
	api.controlSN.Store(math.MaxInt32)
	if got := api.nextControlSN(); got != 1 {
		t.Fatalf("control SN after wrap = %d", got)
	}
}

func TestConfigDownloadUsesUnifiedQuerySN(t *testing.T) {
	api := &GB28181API{}
	api.querySN.Store(40)
	first := string(api.newBasicParamRequest("34020000001320000001"))
	second := string(api.newBasicParamRequest("34020000001320000001"))
	for index, body := range []string{first, second} {
		want := 41 + index
		if !strings.Contains(body, "<SN>"+strconv.Itoa(want)+"</SN>") {
			t.Fatalf("ConfigDownload body does not contain SN %d: %s", want, body)
		}
	}
}
