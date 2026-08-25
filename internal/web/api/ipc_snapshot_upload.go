package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

var errGBSnapshotUploadTooLarge = errors.New("GB28181 snapshot upload is too large")

func readGBSnapshotUploadBody(w http.ResponseWriter, r *http.Request) ([]byte, string, error) {
	if r == nil || r.Body == nil {
		return nil, "", fmt.Errorf("snapshot request body is unavailable")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			return nil, "", errGBSnapshotUploadTooLarge
		}
		return nil, "", err
	}
	payload, payloadType, err := decodeGBSnapshotBody(r, body)
	if err != nil {
		return nil, "", err
	}
	if len(payload) == 0 {
		return nil, "", fmt.Errorf("snapshot payload is empty")
	}
	if len(payload) > maxSnapshotBytes {
		return nil, "", errGBSnapshotUploadTooLarge
	}
	return payload, payloadType, nil
}

// decodeGBSnapshotBody 兼容设备直接上传图片流和 multipart/form-data 两种抓拍回传格式。
func decodeGBSnapshotBody(r *http.Request, body []byte) ([]byte, string, error) {
	contentType := strings.TrimSpace(r.Header.Get("Content-Type"))
	if contentType == "" {
		return body, "raw", nil
	}

	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return body, "raw", nil
	}
	if !strings.EqualFold(mediaType, "multipart/form-data") {
		return body, "raw", nil
	}

	boundary := strings.TrimSpace(params["boundary"])
	if boundary == "" {
		return nil, "", fmt.Errorf("multipart boundary is required")
	}

	reader := multipart.NewReader(bytes.NewReader(body), boundary)
	for {
		part, err := reader.NextPart()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", err
		}
		data, readErr := io.ReadAll(part)
		_ = part.Close()
		if readErr != nil {
			return nil, "", readErr
		}
		if len(data) == 0 {
			continue
		}
		if part.FileName() != "" || strings.EqualFold(part.FormName(), "file") {
			return data, "multipart:file", nil
		}
	}

	return nil, "", fmt.Errorf("multipart file part not found")
}
