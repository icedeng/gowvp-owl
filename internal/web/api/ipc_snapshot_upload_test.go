package api

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http/httptest"
	"testing"
)

func TestReadGBSnapshotUploadBodyEnforcesLimits(t *testing.T) {
	tests := []struct {
		name    string
		size    int
		wantErr bool
	}{
		{name: "accepted", size: maxSnapshotBytes, wantErr: false},
		{name: "decoded payload too large", size: maxSnapshotBytes + 1, wantErr: true},
		{name: "request body too large", size: maxWebhookBodyBytes + 1, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/gb28181/snapshot", bytes.NewReader(make([]byte, test.size)))
			response := httptest.NewRecorder()
			payload, payloadType, err := readGBSnapshotUploadBody(response, request)
			if test.wantErr {
				if !errors.Is(err, errGBSnapshotUploadTooLarge) {
					t.Fatalf("oversized snapshot error = %v; want %v", err, errGBSnapshotUploadTooLarge)
				}
				return
			}
			if err != nil || payloadType != "raw" || len(payload) != test.size {
				t.Fatalf("snapshot body = size:%d type:%q err:%v", len(payload), payloadType, err)
			}
		})
	}
}

func TestReadGBSnapshotUploadBodyExtractsMultipartFile(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "snapshot.jpg")
	if err != nil {
		t.Fatal(err)
	}
	want := []byte("snapshot-image")
	if _, err = part.Write(want); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/gb28181/snapshot", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	payload, payloadType, err := readGBSnapshotUploadBody(httptest.NewRecorder(), request)
	if err != nil || payloadType != "multipart:file" || !bytes.Equal(payload, want) {
		t.Fatalf("multipart snapshot = %q type:%q err:%v", payload, payloadType, err)
	}
}
