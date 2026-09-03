package gbs

import (
	"errors"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

func TestRecordQueryItemsResultDistinguishesEmptyResponseFromTimeout(t *testing.T) {
	if items, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Expected: 0, Complete: true}); err != nil || len(items) != 0 {
		t.Fatalf("complete empty RecordInfo result = %v, %v", items, err)
	}
	if _, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Expected: -1}); err == nil {
		t.Fatal("missing RecordInfo response was reported as an empty recording list")
	}
	partial := []RecordItem{{FilePath: "partial.ps"}}
	items, err := recordQueryItemsResult(multiResponseResult[RecordItem]{Items: partial, Expected: 2})
	var incomplete *RecordInfoIncompleteError
	if !errors.As(err, &incomplete) {
		t.Fatalf("partial RecordInfo result error = %v; want RecordInfoIncompleteError", err)
	}
	if len(items) != 1 || incomplete.Received != 1 || incomplete.Expected != 2 {
		t.Fatalf("partial RecordInfo result = %v, %+v", items, incomplete)
	}
}

func TestRecordInfoMetadataPreservesEmptyAndWhitespaceExtraInfo(t *testing.T) {
	api := &GB28181API{}
	const key = "record-extra-info-whitespace"
	generation := api.startRecordResponseExtra(key)
	body := []byte(`<Response><CmdType>RecordInfo</CmdType>` +
		`<ExtraInfo>  keep  </ExtraInfo><ExtraInfo>   </ExtraInfo><ExtraInfo></ExtraInfo><ExtraInfo> x </ExtraInfo></Response>`)
	objects := api.decodeAppendixA4Objects("RecordInfo", body)
	if err := api.appendRecordResponseMetadata(key, body, objects); err != nil {
		t.Fatal(err)
	}
	metadata := api.takeRecordResponseMetadata(key, generation)
	want := []string{"  keep  ", "   ", "", " x "}
	if !reflect.DeepEqual(metadata.ExtraInfo, want) {
		t.Fatalf("RecordInfo ExtraInfo metadata = %#v, want %#v", metadata.ExtraInfo, want)
	}
}

func TestRecordInfoCompletesOnlyAfterSuccessfulSIPOK(t *testing.T) {
	for _, test := range []struct {
		name     string
		writeErr error
		complete bool
	}{
		{name: "success", complete: true},
		{name: "write failure", writeErr: errors.New("write failed")},
	} {
		t.Run(test.name, func(t *testing.T) {
			api, _ := newVersionGateAPI(GBVersion11)
			collector := newMultiResponseCollector(func(item RecordItem) string { return item.FilePath })
			key := buildMultiResponseKey(gb10DeviceID, "RecordInfo", 98)
			collector.Start(key)
			api.recordResponses = collector
			body := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>98</SN><DeviceID>` + gb10DeviceID + `</DeviceID><Name></Name><SumNum>0</SumNum><RecordList Num="0"></RecordList></Response>`)
			conn, done := startBlockingFlowHandler(t, api, sip.MethodMessage, "record-info-commit-"+test.name, body, api.sipMessageRecordInfo, test.writeErr)

			collector.mu.Lock()
			completedBeforeSIP := collector.entries[key].complete
			collector.mu.Unlock()
			finishBlockingFlowHandler(t, conn, done)
			if completedBeforeSIP {
				t.Fatal("RecordInfo aggregation completed before SIP 200 was written")
			}
			collector.mu.Lock()
			completed := collector.entries[key].complete
			collector.mu.Unlock()
			if completed != test.complete {
				t.Fatalf("RecordInfo aggregation completed = %v, want %v", completed, test.complete)
			}
		})
	}
}

func TestDeviceQueryRecordInfoReturnsActualRequestSN(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion20)
	api.recordResponses = newMultiResponseCollector(func(item RecordItem) string { return item.FilePath })
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	client, peer := net.Pipe()
	connection := sip.NewTCPConnection(client)
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = connection.RemoteAddr()
		device.to = remote
	})
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = peer.Close()
		sipServer.Close()
	})

	type queryResult struct {
		out *DeviceQueryOutput
		err error
	}
	done := make(chan queryResult, 1)
	go func() {
		out, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
			DeviceID: gb10DeviceID, Action: deviceQueryActionRecordInfo,
			Start: 1, End: 2, Timeout: time.Second,
		})
		done <- queryResult{out: out, err: err}
	}()

	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request := string(buffer[:n])
	start := strings.Index(request, "<SN>")
	end := strings.Index(request, "</SN>")
	if start < 0 || end <= start+len("<SN>") {
		t.Fatalf("RecordInfo request has no SN: %s", request)
	}
	requestSN, err := strconv.Atoi(request[start+len("<SN>") : end])
	if err != nil {
		t.Fatalf("parse RecordInfo request SN: %v", err)
	}
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=device-record-info"
	}
	response := "SIP/2.0 200 OK\r\n" +
		"Via: " + cascadeTestHeader(request, "Via") + "\r\n" +
		"From: " + cascadeTestHeader(request, "From") + "\r\n" +
		"To: " + to + "\r\n" +
		"Call-ID: " + cascadeTestHeader(request, "Call-ID") + "\r\n" +
		"CSeq: " + cascadeTestHeader(request, "CSeq") + "\r\n" +
		"Content-Length: 0\r\n\r\n"
	if _, err := peer.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}

	body := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>` + strconv.Itoa(requestSN) + `</SN><DeviceID>` +
		gb10DeviceID + `</DeviceID><Name></Name><SumNum>0</SumNum><RecordList Num="0"></RecordList></Response>`)
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-info-business-response", body, api.sipMessageRecordInfo))

	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.out == nil || result.out.SN != requestSN {
			t.Fatalf("RecordInfo output SN = %+v; want request SN %d", result.out, requestSN)
		}
		if result.out.XML != string(body) {
			t.Fatalf("RecordInfo output XML = %q; want %q", result.out.XML, body)
		}
	case <-time.After(time.Second):
		t.Fatal("RecordInfo DeviceQuery did not complete")
	}
}

func TestDeviceQueryRecordInfoReturnsPartialResultOnTimeout(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion30)
	base, _, _ := newCascadeMediaCore(t)
	api.core = base
	api.recordResponses = newMultiResponseCollector(func(item RecordItem) string { return item.FilePath })
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	client, peer := net.Pipe()
	connection := sip.NewTCPConnection(client)
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = connection.RemoteAddr()
		device.to = remote
	})
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = peer.Close()
		sipServer.Close()
	})

	startTime := time.Date(2026, 8, 29, 8, 0, 0, 0, sip.GBTimeLocation())
	endTime := startTime.Add(2 * time.Hour)
	type queryResult struct {
		out *DeviceQueryOutput
		err error
	}
	done := make(chan queryResult, 1)
	go func() {
		out, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
			DeviceID: gb10DeviceID, Action: deviceQueryActionRecordInfo,
			Start: startTime.Unix(), End: endTime.Unix(), Timeout: 200 * time.Millisecond,
		})
		done <- queryResult{out: out, err: err}
	}()

	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request := string(buffer[:n])
	start := strings.Index(request, "<SN>")
	end := strings.Index(request, "</SN>")
	if start < 0 || end <= start+len("<SN>") {
		t.Fatalf("RecordInfo request has no SN: %s", request)
	}
	requestSN, err := strconv.Atoi(request[start+len("<SN>") : end])
	if err != nil {
		t.Fatalf("parse RecordInfo request SN: %v", err)
	}
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=device-record-info-partial"
	}
	response := "SIP/2.0 200 OK\r\n" +
		"Via: " + cascadeTestHeader(request, "Via") + "\r\n" +
		"From: " + cascadeTestHeader(request, "From") + "\r\n" +
		"To: " + to + "\r\n" +
		"Call-ID: " + cascadeTestHeader(request, "Call-ID") + "\r\n" +
		"CSeq: " + cascadeTestHeader(request, "CSeq") + "\r\n" +
		"Content-Length: 0\r\n\r\n"
	if _, err := peer.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}

	body := []byte(`<Response><CmdType>RecordInfo</CmdType><SN>` + strconv.Itoa(requestSN) + `</SN><DeviceID>` +
		gb10DeviceID + `</DeviceID><Name>device</Name><SumNum>2</SumNum><RecordList Num="1"><Item><DeviceID>` +
		gb10DeviceID + `</DeviceID><Name>record</Name><FilePath>/partial.ps</FilePath><StartTime>` +
		startTime.Format("2006-01-02T15:04:05") + `</StartTime><EndTime>` + startTime.Add(time.Hour).Format("2006-01-02T15:04:05") +
		`</EndTime><Secrecy>0</Secrecy><Type>time</Type></Item></RecordList><ExtraInfo>{"type":"doorType","DeviceID":"` +
		gb10DeviceID + `"}</ExtraInfo></Response>`)
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "record-info-partial-response", body, api.sipMessageRecordInfo))

	select {
	case result := <-done:
		var incomplete *RecordInfoIncompleteError
		if !errors.As(result.err, &incomplete) || incomplete.Received != 1 || incomplete.Expected != 2 {
			t.Fatalf("partial RecordInfo error = %+v, %v", incomplete, result.err)
		}
		if result.out == nil || result.out.SN != requestSN || result.out.XML != string(body) {
			t.Fatalf("partial RecordInfo output = %+v", result.out)
		}
		records, ok := result.out.Data.(*Records)
		if !ok || records.TimeNum != 1 {
			t.Fatalf("partial RecordInfo records = %#v", result.out.Data)
		}
		if len(result.out.AppendixA4) != 1 || result.out.AppendixA4[0].Type != "doorType" {
			t.Fatalf("partial RecordInfo Appendix A.4 = %+v", result.out.AppendixA4)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partial RecordInfo query did not complete after timeout")
	}
}

func TestDeviceQueryConfigDownloadReportsPartialResponseOnTimeout(t *testing.T) {
	api, memory := newVersionGateAPI(GBVersion11)
	local := mustFlowAddress(t, "sip:"+gb10PlatformID+"@192.0.2.20:5060")
	remote := mustFlowAddress(t, "sip:"+gb10DeviceID+"@192.0.2.10:5060")
	sipServer := sip.NewServer(local)
	client, peer := net.Pipe()
	connection := sip.NewTCPConnection(client)
	api.svr = &Server{Server: sipServer, gb: api, memoryStorer: memory, fromAddress: *local}
	memory.device.UpdateRuntime(func(device *Device) {
		device.IsOnline = true
		device.conn = connection
		device.source = connection.RemoteAddr()
		device.to = remote
	})
	go sipServer.ProcessTCPConnection(connection)
	t.Cleanup(func() {
		_ = peer.Close()
		sipServer.Close()
	})

	type queryResult struct {
		out *DeviceQueryOutput
		err error
	}
	done := make(chan queryResult, 1)
	go func() {
		out, err := api.DeviceQuery(t.Context(), &DeviceQueryInput{
			DeviceID: gb10DeviceID, Action: deviceQueryActionConfigDownload,
			ConfigType: "BasicParam/VideoParamOpt", Timeout: 200 * time.Millisecond,
		})
		done <- queryResult{out: out, err: err}
	}()

	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 8192)
	n, err := peer.Read(buffer)
	if err != nil {
		t.Fatal(err)
	}
	request := string(buffer[:n])
	start := strings.Index(request, "<SN>")
	end := strings.Index(request, "</SN>")
	if start < 0 || end <= start+len("<SN>") {
		t.Fatalf("ConfigDownload request has no SN: %s", request)
	}
	requestSN, err := strconv.Atoi(request[start+len("<SN>") : end])
	if err != nil {
		t.Fatalf("parse ConfigDownload request SN: %v", err)
	}
	to := cascadeTestHeader(request, "To")
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=device-config-download"
	}
	response := "SIP/2.0 200 OK\r\n" +
		"Via: " + cascadeTestHeader(request, "Via") + "\r\n" +
		"From: " + cascadeTestHeader(request, "From") + "\r\n" +
		"To: " + to + "\r\n" +
		"Call-ID: " + cascadeTestHeader(request, "Call-ID") + "\r\n" +
		"CSeq: " + cascadeTestHeader(request, "CSeq") + "\r\n" +
		"Content-Length: 0\r\n\r\n"
	if _, err := peer.Write([]byte(response)); err != nil {
		t.Fatal(err)
	}

	body := []byte(`<Response><CmdType>ConfigDownload</CmdType><SN>` + strconv.Itoa(requestSN) + `</SN><DeviceID>` +
		gb10DeviceID + `</DeviceID><Result>OK</Result><BasicParam><Name>IPC</Name><DeviceID>` + gb10DeviceID +
		`</DeviceID><SIPServerID>` + gb10PlatformID + `</SIPServerID><SIPServerIP>192.0.2.20</SIPServerIP>` +
		`<SIPServerPort>5060</SIPServerPort><DomainName>3402000000</DomainName><Expiration>3600</Expiration>` +
		`<Password>secret</Password><HeartBeatInterval>60</HeartBeatInterval><HeartBeatCount>3</HeartBeatCount>` +
		`</BasicParam></Response>`)
	assertFlowOK(t, runFlowHandler(t, newFlowConnection(), api, sip.MethodMessage, "config-download-partial-response", body, api.sipMessageConfigDownload))

	select {
	case result := <-done:
		var incomplete *ConfigDownloadIncompleteError
		if !errors.As(result.err, &incomplete) {
			t.Fatalf("partial ConfigDownload error = %v; want ConfigDownloadIncompleteError", result.err)
		}
		if strings.Join(incomplete.Received, ",") != "BasicParam" || strings.Join(incomplete.Missing, ",") != "VideoParamOpt" {
			t.Fatalf("partial ConfigDownload completion = %+v", incomplete)
		}
		if result.out == nil {
			t.Fatalf("partial ConfigDownload output = %+v", result.out)
		}
		state, ok := result.out.Data.(*ConfigDownloadState)
		if result.out.SN != requestSN || !ok || state.BasicParam == nil {
			t.Fatalf("partial ConfigDownload output = %+v", result.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("partial ConfigDownload query did not complete after timeout")
	}
}

func TestConfigDownloadTimeoutUsesCompletedConcurrentResponse(t *testing.T) {
	pending := &pendingQueryWait{
		expectedConfig: map[string]struct{}{},
		config: &ConfigDownloadState{
			BasicParam:    &BasicParam{Name: "IPC"},
			VideoParamOpt: &VideoParamOpt{InnerXML: "<VideoFormatOpt>2/5</VideoFormatOpt>"},
		},
		responseXML: []string{"<Response><BasicParam/></Response>", "<Response><VideoParamOpt/></Response>"},
	}
	out, err := configDownloadPartialResult(18, gb10DeviceID, pending)
	if err != nil {
		t.Fatalf("concurrently completed ConfigDownload error = %v", err)
	}
	if out == nil {
		t.Fatal("concurrently completed ConfigDownload returned no output")
	}
	state, ok := out.Data.(*ConfigDownloadState)
	if out.SN != 18 || !ok || state.BasicParam == nil || state.VideoParamOpt == nil {
		t.Fatalf("concurrently completed ConfigDownload output = %+v", out)
	}
}

func TestCatalogTimeoutReportsPartialAndConcurrentCompletion(t *testing.T) {
	pending := &pendingQueryWait{
		catalogExpected: 2,
		catalogItems:    []Channels{{ChannelID: gb10ChannelID}},
		catalogAppendixA4: []AppendixA4Object{{
			Type: "personType", RawXML: "<Info type=\"personType\"/>",
		}},
		responseXML: []string{"<Response><DeviceList Num=\"1\"/></Response>"},
	}
	out, err := catalogPartialResult(20, gb10DeviceID, pending)
	var incomplete *CatalogIncompleteError
	if !errors.As(err, &incomplete) || incomplete.Received != 1 || incomplete.Expected != 2 {
		t.Fatalf("partial Catalog completion = %+v, %v", incomplete, err)
	}
	if out == nil {
		t.Fatal("partial Catalog returned no output")
	}
	items, ok := out.Data.([]Channels)
	if out.SN != 20 || !ok || len(items) != 1 || len(out.AppendixA4) != 1 {
		t.Fatalf("partial Catalog output = %+v", out)
	}

	pending.catalogExpected = 1
	out, err = catalogPartialResult(20, gb10DeviceID, pending)
	if err != nil || out == nil {
		t.Fatalf("concurrently completed Catalog output = %+v, %v", out, err)
	}
}

func TestTransRecordListMergesOverlapAndDoesNotCreateMidnightZeroLengthItem(t *testing.T) {
	day := time.Date(2026, 8, 24, 23, 0, 0, 0, sip.GBTimeLocation())
	midnight := time.Date(2026, 8, 25, 0, 0, 0, 0, sip.GBTimeLocation())
	records := transRecordList([][]int64{
		{day.Unix(), day.Add(45 * time.Minute).Unix()},
		{day.Add(30 * time.Minute).Unix(), midnight.Unix()},
		{0},
	})
	if records.DayTotal != 1 || records.TimeNum != 1 || len(records.Data) != 1 || len(records.Data[0].Items) != 1 {
		t.Fatalf("record grouping = %+v", records)
	}
	item := records.Data[0].Items[0]
	if item.Start != day.Unix() || item.End != midnight.Unix()-1 {
		t.Fatalf("merged record = %+v", item)
	}
}

func TestTransRecordItemsSkipsMalformedDeviceTimes(t *testing.T) {
	start := time.Date(2026, 8, 25, 8, 0, 0, 0, sip.GBTimeLocation())
	end := start.Add(time.Hour)
	records := transRecordItems([]RecordItem{
		{StartTime: "invalid", EndTime: end.Format("2006-01-02T15:04:05")},
		{StartTime: start.Format("2006-01-02T15:04:05"), EndTime: end.Format("2006-01-02T15:04:05")},
	}, start.Unix(), end.Unix())
	if records.TimeNum != 1 || len(records.Data) != 1 || len(records.Data[0].Items) != 1 {
		t.Fatalf("record items = %+v", records)
	}
}

func TestRecordResponseMetadataByteLimitIsAtomicAndDeduplicated(t *testing.T) {
	api := &GB28181API{}
	const key = "device:RecordInfo:metadata-bytes"
	generation := api.startRecordResponseExtra(key)
	const chunks = 8
	chunkSize := gbRecordResponseMaxMetadataBytes / chunks
	lastBody := ""
	for index := 0; index < chunks; index++ {
		body := strings.Repeat("x", chunkSize-1) + strconv.Itoa(index)
		if err := api.appendRecordResponseMetadata(key, []byte(body), nil); err != nil {
			t.Fatalf("append metadata chunk %d: %v", index, err)
		}
		lastBody = body
	}
	if err := api.appendRecordResponseMetadata(key, []byte(lastBody), nil); err != nil {
		t.Fatalf("duplicate metadata consumed budget: %v", err)
	}
	if err := api.appendRecordResponseMetadata(key, []byte("overflow"), nil); !errors.Is(err, errRecordResponseMetadataLimit) {
		t.Fatalf("metadata byte overflow error = %v", err)
	}

	metadata := api.takeRecordResponseMetadata(key, generation)
	if len(metadata.ResponseXML) != chunks {
		t.Fatalf("metadata XML count = %d; want %d", len(metadata.ResponseXML), chunks)
	}
	api.recordResponseExtraMu.Lock()
	_, usageExists := api.recordResponseMetadata[key]
	api.recordResponseExtraMu.Unlock()
	if usageExists {
		t.Fatal("metadata usage survived result consumption")
	}
}

func TestRecordResponseMetadataObjectLimitIsAtomicAndDeduplicated(t *testing.T) {
	api := &GB28181API{}
	const key = "device:RecordInfo:metadata-objects"
	generation := api.startRecordResponseExtra(key)
	objects := make([]AppendixA4Object, gbRecordResponseMaxMetadataObjects)
	for index := range objects {
		objects[index] = AppendixA4Object{
			Type:   "vendorType" + strconv.Itoa(index),
			Path:   "/Response/Info",
			RawXML: "<Info>" + strconv.Itoa(index) + "</Info>",
		}
	}
	if err := api.appendRecordResponseMetadata(key, nil, objects); err != nil {
		t.Fatalf("append metadata objects: %v", err)
	}
	if err := api.appendRecordResponseMetadata(key, nil, objects[:1]); err != nil {
		t.Fatalf("duplicate metadata object consumed budget: %v", err)
	}
	overflow := AppendixA4Object{Type: "overflowType", Path: "/Response/Info", RawXML: "<Info>overflow</Info>"}
	if err := api.appendRecordResponseMetadata(key, nil, []AppendixA4Object{overflow}); !errors.Is(err, errRecordResponseMetadataLimit) {
		t.Fatalf("metadata object overflow error = %v", err)
	}

	metadata := api.takeRecordResponseMetadata(key, generation)
	if len(metadata.AppendixA4) != gbRecordResponseMaxMetadataObjects {
		t.Fatalf("metadata Appendix A.4 count = %d; want %d", len(metadata.AppendixA4), gbRecordResponseMaxMetadataObjects)
	}
	for _, value := range metadata.AppendixA4 {
		if value.Type == overflow.Type {
			t.Fatal("overflowing metadata object was partially committed")
		}
	}
}
