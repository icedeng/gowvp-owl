package api

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"
)

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
