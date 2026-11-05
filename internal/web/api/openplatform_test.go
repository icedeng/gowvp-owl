package api

import (
	"bytes"
	"context"
	"github.com/glebarez/sqlite"
	"github.com/gowvp/owl/internal/core/capture"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/gowvp/owl/internal/conf"
	"github.com/ixugo/goddd/pkg/web"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAdminRequiresCurrentAdministratorJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &conf.Bootstrap{}
	cfg.Server.Username = "admin"
	cfg.Server.HTTP.JwtSecret = "test-only-admin-key"
	cfg.Server.HTTP.AuthURL = "https://must-not-be-called.invalid"
	uc := &Usecase{Conf: cfg, UserAPI: NewUserAPI(cfg)}
	valid, _ := web.NewToken(web.NewClaimsData().SetUsername("admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(time.Hour)))
	old, _ := web.NewToken(web.NewClaimsData().SetUsername("old-admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(time.Hour)))
	expired, _ := web.NewToken(web.NewClaimsData().SetUsername("admin"), cfg.Server.HTTP.JwtSecret, web.WithExpiresAt(time.Now().Add(-time.Hour)))
	r := gin.New()
	r.GET("/test", strictOpenAdmin(uc), func(c *gin.Context) { c.Status(204) })
	for _, tc := range []struct {
		auth, path string
		status     int
	}{{"Bearer " + valid, "/test", 204}, {"Bearer " + old, "/test", 401}, {"Bearer " + expired, "/test", 401}, {"", "/test?token=" + valid, 401}, {"OWL-HMAC-SHA256 Credential=owl_ak_fake,Signature=fake", "/test", 401}, {"", "/test", 401}} {
		out := httptest.NewRecorder()
		req := httptest.NewRequest("GET", tc.path, nil)
		req.Header.Set("Authorization", tc.auth)
		r.ServeHTTP(out, req)
		if out.Code != tc.status {
			t.Fatal(tc.status, out.Code)
		}
	}
}

func TestOpenSnapshotReusesPersistentCapture(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "capture.db")), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := db.DB()
	raw.SetMaxOpenConns(1)
	defer raw.Close()
	if err = db.AutoMigrate(&capture.Record{}); err != nil {
		t.Fatal(err)
	}
	service := capture.New(db, t.TempDir())
	a := IPCAPI{captures: service}
	host := openHost{uc: &Usecase{GB28181API: a}}
	var imageBody bytes.Buffer
	jpeg.Encode(&imageBody, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.Run(ctx, func(ctx context.Context, row *capture.Record) error {
			_, err := service.Complete(ctx, row.ID, imageBody.Bytes(), "test-upload")
			return err
		})
	}()
	defer func() { cancel(); <-done }()
	dest := filepath.Join(t.TempDir(), "open.jpg")
	wait, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	if err = host.Snapshot(wait, "channel-1", dest); err != nil {
		t.Fatal(err)
	}
	rows, total, err := service.List(ctx, "channel-1", 1, 10)
	if err != nil || total != 1 || rows[0].Source != "openapi" || rows[0].State != "succeeded" {
		t.Fatal(rows, total, err)
	}
	path, err := host.CapturePath(ctx, rows[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stored, _ := os.ReadFile(path)
	copied, _ := os.ReadFile(dest)
	if !bytes.Equal(stored, copied) || !bytes.Equal(copied, imageBody.Bytes()) {
		t.Fatal("snapshot copy did not match saved persistent image")
	}
}
