package api

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/pkg/thumbnail"
	"github.com/ixugo/goddd/pkg/orm"
	"github.com/ixugo/goddd/pkg/system"
	"github.com/ixugo/goddd/pkg/web"
)

func TestEventThumbnailAuthenticationLazyGenerationAndOriginal(t *testing.T) {
	t.Chdir(t.TempDir())
	root := filepath.Join(system.Getwd(), "configs", "events")
	if err := os.MkdirAll(root, 0750); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "thumbnail-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	var body bytes.Buffer
	if err := jpeg.Encode(&body, image.NewRGBA(image.Rect(0, 0, 1600, 900)), nil); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "one.jpg")
	if err := os.WriteFile(original, body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	r := gin.New()
	RegisterEvent(r, EventAPI{}, AuthMiddleware("thumbnail-test", ""))
	token, err := web.NewToken(web.NewClaimsData().SetUsername("preview"), "thumbnail-test")
	if err != nil {
		t.Fatal(err)
	}
	request := func(path string, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		out := httptest.NewRecorder()
		r.ServeHTTP(out, req)
		return out
	}
	path := "/events/image/" + filepath.Base(dir) + "/one.jpg"
	if out := request(path+"?thumbnail=true", false); out.Code != 401 {
		t.Fatal("缩略图未鉴权", out.Code)
	}
	out := request(path+"?thumbnail=true", true)
	preview, err := jpeg.Decode(bytes.NewReader(out.Body.Bytes()))
	if out.Code != 200 || err != nil || preview.Bounds().Size() != image.Pt(640, 360) {
		t.Fatal("历史事件缩略图读取失败", out.Code, err)
	}
	if _, err := os.Stat(thumbnail.Path(original)); err != nil {
		t.Fatal("未保存历史事件缩略图", err)
	}
	if out := request(path, true); out.Code != 200 || !bytes.Equal(out.Body.Bytes(), body.Bytes()) {
		t.Fatal("查看原图没有返回原始字节", out.Code)
	}
	// 新的 Webhook 抓拍在写入时就应生成缩略图。
	saved, err := saveEventSnapshot("thumbnail-camera", orm.Now(), base64.StdEncoding.EncodeToString(body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = thumbnail.Remove(filepath.Join(root, saved)) })
	if _, err := os.Stat(thumbnail.Path(filepath.Join(root, saved))); err != nil {
		t.Fatal("新事件图片未生成缩略图", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jpg")
	if err := os.WriteFile(outside, body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.jpg")); err != nil {
		t.Fatal(err)
	}
	if out := request("/events/image/"+filepath.Base(dir)+"/escape.jpg?thumbnail=true", true); out.Code == 200 {
		t.Fatal("缩略图接口允许符号链接越界")
	}
	if _, err := os.Stat(thumbnail.Path(outside)); !os.IsNotExist(err) {
		t.Fatal("越界文件生成了缩略图", err)
	}
}
