package upgrader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testPackage(t *testing.T, pub ed25519.PublicKey, private ed25519.PrivateKey) (Manifest, []byte) {
	t.Helper()
	var image bytes.Buffer
	tw := tar.NewWriter(&image)
	id := strings.Repeat("a", 64)
	config := []byte(`{"architecture":"amd64","os":"linux","config":{"Labels":{"org.opencontainers.image.version":"v2.5.0"}}}`)
	configDigest := sha256.Sum256(config)
	configName := "blobs/sha256/" + hex.EncodeToString(configDigest[:])
	_ = tw.WriteHeader(&tar.Header{Name: configName, Mode: 0600, Size: int64(len(config)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(config)
	// 模拟Docker 29：镜像ID使用清单摘要，配置使用独立摘要。
	entry, _ := json.Marshal([]map[string]any{{"Config": configName, "RepoTags": []string{"owl-v1-service:v2.5.0"}, "Layers": []string{}}})
	_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(entry)), Typeflag: tar.TypeReg})
	_, _ = tw.Write(entry)
	_ = tw.Close()
	h := sha256.Sum256(image.Bytes())
	m := Manifest{Format: 1, Product: "owl-docker", Version: "v2.5.0", OS: "linux", Architecture: "amd64", Image: "owl-v1-service:v2.5.0", ImageID: "sha256:" + id, ImageSHA256: hex.EncodeToString(h[:]), ImageSize: int64(image.Len()), FromVersions: []string{"v2.4.1-openapisnapshot1"}, Notes: "test"}
	b, _ := json.Marshal(m)
	sig := ed25519.Sign(private, b)
	var out bytes.Buffer
	z := gzip.NewWriter(&out)
	tw = tar.NewWriter(z)
	for _, item := range []struct {
		name string
		data []byte
	}{{"manifest.json", b}, {"manifest.sig", sig}, {"image.tar", image.Bytes()}} {
		_ = tw.WriteHeader(&tar.Header{Name: item.name, Mode: 0600, Size: int64(len(item.data)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(item.data)
	}
	_ = tw.Close()
	_ = z.Close()
	return m, out.Bytes()
}

func TestPackageSignatureDigestAndCompatibility(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m, b := testPackage(t, pub, priv)
	dir := t.TempDir()
	got, err := Extract(bytes.NewReader(b), dir, pub)
	if err != nil || got.Version != m.Version {
		t.Fatalf("valid package: %v", err)
	}
	if !got.CanInstall("v2.4.1-openapisnapshot1") || got.CanInstall("v2.5.0") || got.CanInstall("v3.0.0") || got.CanInstall("dev") {
		t.Fatal("version gate failed")
	}
	reset := got
	reset.Version, reset.Image = "v2.0.2", "owl-v1-service:v2.0.2"
	if reset.CanInstall("v2.4.1-openapisnapshot1") {
		t.Fatal("implicit downgrade allowed")
	}
	reset.ReplacesVersion = "v2.4.1-openapisnapshot1"
	if reset.Validate() != nil || !reset.CanInstall("v2.4.1-openapisnapshot1") || reset.CanInstall("v2.0.2") || reset.CanInstall("v2.4.1") {
		t.Fatal("version reset gate failed")
	}
	reset.FromVersions = append(reset.FromVersions, "v2.4.1")
	if reset.Validate() == nil {
		t.Fatal("broad version reset allowed")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Verify(dir, other); err == nil {
		t.Fatal("untrusted signature accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "image.tar"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, pub); err == nil {
		t.Fatal("modified image accepted")
	}
	m.Migration = true
	if m.Validate() == nil {
		t.Fatal("migration package accepted")
	}
	m.Migration = false
	m.Image = "postgres:v2.5.0"
	if m.Validate() == nil {
		t.Fatal("foreign image accepted")
	}
}

func TestPackageRejectsPathsLinksDuplicatesAndTruncation(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	for _, name := range []string{"../escape", "/absolute", "release.sh", "image.tar"} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			z := gzip.NewWriter(&b)
			tw := tar.NewWriter(z)
			typeflag := byte(tar.TypeReg)
			if name == "image.tar" {
				typeflag = tar.TypeSymlink
			}
			_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: 1, Typeflag: typeflag, Linkname: "/etc/passwd"})
			_, _ = tw.Write([]byte("x"))
			_ = tw.Close()
			_ = z.Close()
			if _, err := Extract(bytes.NewReader(b.Bytes()), t.TempDir(), pub); err == nil {
				t.Fatal("illegal tar accepted")
			}
		})
	}
	_, b := testPackage(t, pub, priv)
	if _, err := Extract(bytes.NewReader(b[:len(b)-8]), t.TempDir(), pub); err == nil {
		t.Fatal("truncated gzip accepted")
	}
	var duplicate bytes.Buffer
	z := gzip.NewWriter(&duplicate)
	tw := tar.NewWriter(z)
	for range 2 {
		_ = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: 1, Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte("x"))
	}
	_ = tw.Close()
	_ = z.Close()
	if _, err := Extract(bytes.NewReader(duplicate.Bytes()), t.TempDir(), pub); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func setupAgent(t *testing.T, failure string) (*Agent, Release, *string, *string) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	m, b := testPackage(t, pub, priv)
	root := t.TempDir()
	for _, dir := range []string{"backups", "data/configs", "data/zlm-config"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".env", "docker-compose.yml", "data/configs/config.toml", "data/zlm-config/config.ini"} {
		content := "config"
		if name == ".env" {
			content = "OWL_SERVICE_IMAGE_TAG=v2.4.1-openapisnapshot1\nKEEP=unchanged\n"
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	version := "v2.4.1-openapisnapshot1"
	image := "sha256:" + strings.Repeat("b", 64)
	var stateMu sync.Mutex
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]string{"version": version})
	}))
	t.Cleanup(health.Close)
	a := &Agent{Root: root, StateDir: filepath.Join(root, "upgrader"), LockPath: filepath.Join(root, "release.lock"), PublicKey: pub, HealthURL: health.URL}
	if err := a.Init(); err != nil {
		t.Fatal(err)
	}
	id := newID()
	dir, _ := packageDir(a.StateDir, id)
	_ = os.Mkdir(dir, 0700)
	if _, err := Extract(bytes.NewReader(b), dir, pub); err != nil {
		t.Fatal(err)
	}
	rel := Release{id, m}
	_ = atomicJSON(filepath.Join(dir, "release.json"), rel)
	loaded := false
	a.runner = func(ctx context.Context, in io.Reader, out io.Writer, args ...string) ([]byte, error) {
		stateMu.Lock()
		defer stateMu.Unlock()
		cmd := strings.Join(args, " ")
		switch {
		case strings.Contains(cmd, "info --format"):
			return []byte(root), nil
		case strings.Contains(cmd, "pg_dump"):
			if failure == "backup" {
				return nil, errors.New("backup failed")
			}
			_, _ = out.Write([]byte("backup"))
			return nil, nil
		case args[0] == "tar":
			_, _ = out.Write([]byte("config backup"))
			return nil, nil
		case strings.Contains(cmd, "image inspect "+m.Image):
			if !loaded {
				return nil, errors.New("not found")
			}
			if args[len(args)-1] == "{{.Id}}" {
				return []byte(m.ImageID), nil
			}
			return []byte(m.ImageID + "|amd64|linux\n"), nil
		case strings.Contains(cmd, "load --input"):
			if failure == "import" {
				return nil, errors.New("import failed")
			}
			loaded = true
			return nil, nil
		case strings.Contains(cmd, "compose"):
			env, _ := os.ReadFile(filepath.Join(root, ".env"))
			if strings.Contains(string(env), "TAG=v2.5.0") {
				image = m.ImageID
				if failure == "health" || failure == "rollback" {
					version = "wrong-version"
				} else {
					version = m.Version
				}
				if failure == "start" {
					return nil, errors.New("start failed")
				}
			} else {
				if failure == "rollback" {
					return nil, errors.New("rollback failed")
				}
				version = "v2.4.1-openapisnapshot1"
				image = "sha256:" + strings.Repeat("b", 64)
			}
			return nil, nil
		case strings.Contains(cmd, "inspect owl-v1-gowvp-1"):
			return []byte(image), nil
		case strings.Contains(cmd, "inspect owl-v1-postgres-1"):
			return []byte("dependencies unchanged"), nil
		default:
			return nil, nil
		}
	}
	return a, rel, &version, &image
}

func TestCorruptTaskBlocksInstallAndRestart(t *testing.T) {
	a, _, _, _ := setupAgent(t, "")
	if err := os.WriteFile(filepath.Join(a.StateDir, "current.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Init(); err == nil {
		t.Fatal("corrupt task ignored on restart")
	}
	for _, endpoint := range []string{"/install", "/packages"} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("POST", endpoint, strings.NewReader(`{}`)))
		if w.Code != 409 {
			t.Fatalf("%s: corrupt task allowed action: %d", endpoint, w.Code)
		}
	}
}

func TestInstallBackupsHealthAndRollback(t *testing.T) {
	for _, failure := range []string{"", "backup", "import", "start", "health", "rollback"} {
		t.Run(failure, func(t *testing.T) {
			a, rel, version, _ := setupAgent(t, failure)
			lock, err := a.lock()
			if err != nil {
				t.Fatal(err)
			}
			a.mu.Lock()
			task := &Task{ID: newID(), Version: rel.Version, PackageID: rel.ID}
			a.install(task, rel, lock)
			got := a.readTask()
			want := "succeeded"
			switch failure {
			case "backup", "import":
				want = "failed"
			case "start", "health":
				want = "rolled_back"
			case "rollback":
				want = "rollback_failed"
			}
			if got.Phase != want {
				t.Fatalf("got %s want %s: %+v", got.Phase, want, got)
			}
			env, _ := os.ReadFile(filepath.Join(a.Root, ".env"))
			if !strings.Contains(string(env), "KEEP=unchanged") {
				t.Fatal("unrelated environment changed")
			}
			if failure != "" && failure != "rollback" && *version != "v2.4.1-openapisnapshot1" {
				t.Fatal("old service was not restored")
			}
			if failure == "" {
				if got.Backup == "" {
					t.Fatal("backup missing")
				}
				for _, name := range []string{".env", "config.tar.gz", "gowvp.dump"} {
					if _, err := os.Stat(filepath.Join(a.Root, "backups", got.Backup, name)); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestInstallLockAndRecovery(t *testing.T) {
	a, rel, _, _ := setupAgent(t, "")
	lock, err := a.lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.lock(); err == nil {
		t.Fatal("concurrent release lock acquired")
	}
	unlock(lock)
	task := &Task{ID: newID(), Version: rel.Version, PackageID: rel.ID, Phase: "backing_up"}
	_ = a.save(task, "backing_up", "backup")
	if err := a.recoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if a.readTask().Phase != "failed" {
		t.Fatal("pre-install interruption not recorded")
	}
	task = &Task{ID: newID(), Version: rel.Version, PackageID: rel.ID}
	lock, _ = a.lock()
	a.mu.Lock()
	a.install(task, rel, lock)
	_ = a.save(task, "checking", "interrupted")
	if err := a.recoverInterrupted(); err != nil {
		t.Fatal(err)
	}
	if a.readTask().Phase != "rolled_back" {
		t.Fatal("post-switch interruption not rolled back")
	}
}
