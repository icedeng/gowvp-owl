package upgrader

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Task struct {
	ID              string      `json:"id"`
	PackageID       string      `json:"package_id"`
	Version         string      `json:"version"`
	PreviousVersion string      `json:"previous_version"`
	PreviousImage   string      `json:"previous_image"`
	TargetImage     string      `json:"target_image"`
	Phase           string      `json:"phase"`
	Message         string      `json:"message"`
	StartedAt       time.Time   `json:"started_at"`
	UpdatedAt       time.Time   `json:"updated_at"`
	Backup          string      `json:"backup,omitempty"`
	Events          []TaskEvent `json:"events"`
}
type TaskEvent struct {
	Time    time.Time `json:"time"`
	Phase   string    `json:"phase"`
	Message string    `json:"message"`
}

type Agent struct {
	Root, StateDir, LockPath string
	PublicKey                ed25519.PublicKey
	HealthURL                string
	mu                       sync.Mutex
	// runner用于离线故障注入；生产始终使用exec.CommandContext，不运行shell。
	runner func(context.Context, io.Reader, io.Writer, ...string) ([]byte, error)
}

func active(phase string) bool {
	return phase == "queued" || phase == "validating" || phase == "backing_up" || phase == "importing" || phase == "installing" || phase == "checking" || phase == "rolling_back"
}

func (a *Agent) Init() error {
	for _, dir := range []string{a.StateDir, filepath.Join(a.StateDir, "catalog"), filepath.Join(a.StateDir, "tasks")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
	}
	if _, err := a.checkedTask(); err != nil {
		return err
	}
	return a.recoverInterrupted()
}

func (a *Agent) checkedTask() (*Task, error) {
	b, err := os.ReadFile(filepath.Join(a.StateDir, "current.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t Task
	if json.Unmarshal(b, &t) != nil || !idPattern.MatchString(t.ID) || t.Phase == "" {
		return nil, errors.New("升级任务记录损坏，请人工检查，禁止继续安装")
	}
	return &t, nil
}

func (a *Agent) readTask() *Task {
	t, err := a.checkedTask()
	if err != nil {
		return &Task{Phase: "rollback_failed", Message: "升级记录无法读取，请联系运维，禁止继续安装"}
	}
	return t
}

func (a *Agent) save(t *Task, phase, message string) error {
	t.Phase, t.Message, t.UpdatedAt = phase, message, time.Now().UTC()
	t.Events = append(t.Events, TaskEvent{t.UpdatedAt, phase, message})
	if err := atomicJSON(filepath.Join(a.StateDir, "tasks", t.ID+".json"), t); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(a.StateDir, "current.json"), t)
}

func (a *Agent) lock() (*os.File, error) {
	f, err := os.OpenFile(a.LockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("另一个发布任务正在执行")
	}
	return f, nil
}
func unlock(f *os.File) { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }

func (a *Agent) run(ctx context.Context, input io.Reader, output io.Writer, args ...string) ([]byte, error) {
	if a.runner != nil {
		return a.runner(ctx, input, output, args...)
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = a.Root
	cmd.Stdin = input
	// stdout只在pg_dump时直接落盘；错误输出不回传凭据或服务环境。
	if output != nil {
		cmd.Stdout = output
		err := cmd.Run()
		return nil, err
	}
	return cmd.Output()
}

func (a *Agent) docker(ctx context.Context, args ...string) ([]byte, error) {
	return a.run(ctx, nil, nil, append([]string{"docker"}, args...)...)
}
func (a *Agent) compose(ctx context.Context) error {
	_, err := a.docker(ctx, "compose", "-p", "owl-v1", "--env-file", ".env", "-f", "docker-compose.yml", "up", "-d", "--no-deps", "--no-build", "--pull", "never", "--wait", "--wait-timeout", "180", "gowvp")
	return err
}
func (a *Agent) health() (string, error) {
	client := http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(a.HealthURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", errors.New("健康接口失败")
	}
	var out struct {
		Version string `json:"version"`
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	return out.Version, err
}
func (a *Agent) dependencies(ctx context.Context) (string, error) {
	b, err := a.docker(ctx, "inspect", "owl-v1-postgres-1", "owl-v1-zlm-1", "--format", "{{.Id}}|{{.State.StartedAt}}")
	return string(b), err
}

func (a *Agent) configDigests() (map[string]string, error) {
	out := map[string]string{}
	for _, name := range []string{"docker-compose.yml", "data/configs/config.toml", "data/zlm-config/config.ini"} {
		b, err := os.ReadFile(filepath.Join(a.Root, name))
		if err != nil {
			return nil, err
		}
		h := sha256.Sum256(b)
		out[name] = hex.EncodeToString(h[:])
	}
	return out, nil
}

func sameConfig(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func (a *Agent) backup(ctx context.Context, t *Task) error {
	dir := filepath.Join(a.Root, "backups", "web-"+t.ID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	t.Backup = "web-" + t.ID
	for _, name := range []string{".env", "docker-compose.yml", "data/configs", "data/zlm-config"} {
		// configs内的媒体目录是独立大卷，不进入配置备份。
		if _, err := os.Stat(filepath.Join(a.Root, name)); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, "config.tar.gz"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = a.run(ctx, nil, f, "tar", "--exclude=data/configs/recordings", "--exclude=data/configs/downloads", "--exclude=data/configs/events", "--exclude=data/configs/cover", "-czf", "-", ".env", "docker-compose.yml", "data/configs", "data/zlm-config")
	err = errors.Join(err, f.Sync(), f.Close())
	if err != nil {
		return errors.New("配置备份失败")
	}
	env, err := os.ReadFile(filepath.Join(a.Root, ".env"))
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, ".env"), env); err != nil {
		return err
	}
	dump, err := os.OpenFile(filepath.Join(dir, "gowvp.dump"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = a.run(ctx, nil, dump, "docker", "exec", "owl-v1-postgres-1", "pg_dump", "-U", "postgres", "-d", "gowvp", "-Fc", "--no-owner", "--no-acl")
	err = errors.Join(err, dump.Sync(), dump.Close())
	if err != nil {
		return errors.New("数据库备份失败")
	}
	input, err := os.Open(filepath.Join(dir, "gowvp.dump"))
	if err != nil {
		return err
	}
	defer input.Close()
	if _, err = a.run(ctx, input, nil, "docker", "exec", "-i", "owl-v1-postgres-1", "pg_restore", "--list"); err != nil {
		return errors.New("数据库备份校验失败")
	}
	return nil
}

func (a *Agent) rollback(ctx context.Context, t *Task) error {
	if t.Backup != "web-"+t.ID || !idPattern.MatchString(t.ID) {
		return errors.New("回滚备份标识无效")
	}
	// 只恢复应用标签；数据库无迁移，不自动覆盖正在使用的数据库及配置。
	b, err := os.ReadFile(filepath.Join(a.Root, "backups", t.Backup, ".env"))
	if err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(a.Root, ".env"), b); err != nil {
		return err
	}
	if err := a.compose(ctx); err != nil {
		return err
	}
	v, err := a.health()
	if err != nil || v != t.PreviousVersion {
		return errors.New("旧版本健康检查失败")
	}
	image, err := a.docker(ctx, "inspect", "owl-v1-gowvp-1", "--format", "{{.Image}}")
	if err != nil || strings.TrimSpace(string(image)) != t.PreviousImage {
		return errors.New("旧镜像校验失败")
	}
	return nil
}

func (a *Agent) recoverInterrupted() error {
	t := a.readTask()
	if t == nil || !active(t.Phase) {
		return nil
	}
	f, err := a.lock()
	if err != nil {
		return err
	}
	defer unlock(f)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if t.Phase == "installing" || t.Phase == "checking" || t.Phase == "rolling_back" {
		image, err := a.docker(ctx, "inspect", "owl-v1-gowvp-1", "--format", "{{.Image}}")
		current := strings.TrimSpace(string(image))
		if err != nil || (current != t.TargetImage && current != t.PreviousImage) {
			return a.save(t, "rollback_failed", "升级服务中断，现有镜像已变化，请人工检查")
		}
		if err := a.rollback(ctx, t); err != nil {
			return a.save(t, "rollback_failed", "中断恢复失败，请人工检查备份和服务")
		}
		return a.save(t, "rolled_back", "升级服务重启，已恢复升级前版本")
	}
	return a.save(t, "failed", "升级服务重启，安装尚未开始，请重新发起")
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

func (a *Agent) install(t *Task, release Release, lock *os.File) {
	defer a.mu.Unlock()
	defer unlock(lock)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	changed := false
	step := func(phase, message string) error { return a.save(t, phase, message) }
	fail := func(message string) {
		if !changed {
			_ = a.save(t, "failed", message)
			return
		}
		_ = a.save(t, "rolling_back", message+"，正在恢复旧版本")
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer rcancel()
		if err := a.rollback(rctx, t); err != nil {
			_ = a.save(t, "rollback_failed", "自动回滚失败，请联系运维，备份编号："+t.Backup)
			return
		}
		_ = a.save(t, "rolled_back", message+"，已恢复旧版本")
	}
	defer func() {
		if err := recover(); err != nil {
			slog.Error("升级任务异常", "task", t.ID)
			fail("升级任务异常中断")
		}
	}()
	if err := step("validating", "复核签名、镜像摘要和当前版本"); err != nil {
		fail("任务记录写入失败")
		return
	}
	dir, _ := packageDir(a.StateDir, release.ID)
	m, err := Verify(dir, a.PublicKey)
	if err != nil {
		fail("发布包复核失败")
		return
	}
	if m.Version != release.Version || m.ImageID != release.ImageID {
		fail("发布包记录与签名清单不匹配")
		return
	}
	current, err := a.health()
	if err != nil || !m.CanInstall(current) {
		fail("当前版本不在此发布包的兼容升级范围")
		return
	}
	t.PreviousVersion = current
	old, err := a.docker(ctx, "inspect", "owl-v1-gowvp-1", "--format", "{{.Image}}")
	if err != nil {
		fail("读取当前应用镜像失败")
		return
	}
	t.PreviousImage = strings.TrimSpace(string(old))
	t.TargetImage = m.ImageID
	deps, err := a.dependencies(ctx)
	if err != nil {
		fail("读取依赖状态失败")
		return
	}
	configs, err := a.configDigests()
	if err != nil {
		fail("读取配置摘要失败")
		return
	}
	dockerRoot, err := a.docker(ctx, "info", "--format", "{{.DockerRootDir}}")
	var disk syscall.Statfs_t
	if err != nil || syscall.Statfs(strings.TrimSpace(string(dockerRoot)), &disk) != nil || uint64(disk.Bavail)*uint64(disk.Bsize) < uint64(m.ImageSize*2+1<<30) {
		fail("Docker存储空间不足或无法检查")
		return
	}
	if _, err := a.run(ctx, nil, nil, "./check-storage.sh"); err != nil {
		fail("存储检查失败")
		return
	}
	if err := step("backing_up", "备份配置和数据库，校验备份可读性"); err != nil {
		fail("任务记录写入失败")
		return
	}
	if err := a.backup(ctx, t); err != nil {
		fail("配置或数据库备份失败，未修改应用")
		return
	}
	if err := atomicJSON(filepath.Join(a.Root, "backups", t.Backup, "config-digests.json"), configs); err != nil {
		fail("保存配置摘要失败")
		return
	}
	if err := step("importing", "导入已验证的应用镜像"); err != nil {
		fail("任务记录写入失败")
		return
	}
	if existing, err := a.docker(ctx, "image", "inspect", m.Image, "--format", "{{.Id}}"); err == nil {
		if strings.TrimSpace(string(existing)) != m.ImageID {
			fail("目标镜像标签已被其他镜像占用，禁止覆盖")
			return
		}
	} else {
		if _, err := a.docker(ctx, "load", "--input", filepath.Join(dir, "image.tar")); err != nil {
			fail("镜像导入失败")
			return
		}
	}
	data, err := a.docker(ctx, "image", "inspect", m.Image, "--format", "{{.Id}}|{{.Architecture}}|{{.Os}}")
	if err != nil || strings.TrimSpace(string(data)) != m.ImageID+"|amd64|linux" {
		fail("导入镜像的标识或架构不匹配")
		return
	}
	env, err := os.ReadFile(filepath.Join(a.Root, ".env"))
	if err != nil {
		fail("读取应用环境失败")
		return
	}
	before, err := os.ReadFile(filepath.Join(a.Root, "backups", t.Backup, ".env"))
	if err != nil || string(env) != string(before) {
		fail("环境文件在备份后发生变化，停止升级")
		return
	}
	tagLine := regexp.MustCompile(`(?m)^OWL_SERVICE_IMAGE_TAG=.*$`)
	if len(tagLine.FindAll(env, -1)) != 1 {
		fail("应用镜像标签配置不唯一")
		return
	}
	beforeSwitch, err := a.configDigests()
	if err != nil || !sameConfig(configs, beforeSwitch) {
		fail("配置在备份后发生变化，停止升级")
		return
	}
	// durable installing必须先于env切换，宿主机服务重启后才能恢复。
	if err := step("installing", "切换应用镜像，实时预览会短暂中断"); err != nil {
		fail("任务记录写入失败")
		return
	}
	changed = true
	if err := atomicWrite(filepath.Join(a.Root, ".env"), tagLine.ReplaceAllLiteral(env, []byte("OWL_SERVICE_IMAGE_TAG="+m.Version))); err != nil {
		fail("镜像标签更新失败")
		return
	}
	if err := a.compose(ctx); err != nil {
		fail("应用启动失败")
		return
	}
	if err := step("checking", "核对新版本健康状态及依赖服务"); err != nil {
		fail("任务记录写入失败")
		return
	}
	v, err := a.health()
	if err != nil || v != m.Version {
		fail("新版本健康检查不匹配")
		return
	}
	image, err := a.docker(ctx, "inspect", "owl-v1-gowvp-1", "--format", "{{.Image}}")
	if err != nil || strings.TrimSpace(string(image)) != m.ImageID {
		fail("运行镜像校验失败")
		return
	}
	after, err := a.dependencies(ctx)
	if err != nil || after != deps {
		fail("依赖容器状态发生变化")
		return
	}
	configAfter, err := a.configDigests()
	if err != nil || !sameConfig(configs, configAfter) {
		fail("升级后业务配置发生变化")
		return
	}
	if err := step("succeeded", "升级完成，健康检查通过，请刷新页面加载新版本"); err != nil {
		fail("完成记录保存失败")
	}
}

func (a *Agent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	reply := func(status int, v any) { w.WriteHeader(status); _ = json.NewEncoder(w).Encode(v) }
	errReply := func(status int, msg string) { reply(status, map[string]string{"msg": msg}) }
	switch {
	case r.Method == "GET" && r.URL.Path == "/status":
		releases := []Release{}
		entries, _ := os.ReadDir(filepath.Join(a.StateDir, "catalog"))
		for _, entry := range entries {
			if !entry.IsDir() || !idPattern.MatchString(entry.Name()) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(a.StateDir, "catalog", entry.Name(), "release.json"))
			var rel Release
			if err == nil && json.Unmarshal(b, &rel) == nil {
				releases = append(releases, rel)
			}
		}
		reply(200, map[string]any{"available": true, "source": "本平台签名发布库", "max_upload_bytes": MaxUpload, "releases": releases, "task": a.readTask()})
	case r.Method == "POST" && r.URL.Path == "/packages":
		if !a.mu.TryLock() {
			errReply(409, "正在上传或安装，请稍后重试")
			return
		}
		defer a.mu.Unlock()
		if t := a.readTask(); t != nil && (active(t.Phase) || t.Phase == "rollback_failed") {
			errReply(409, "存在未完成的升级任务")
			return
		}
		if r.ContentLength <= 0 || r.ContentLength > MaxUpload {
			errReply(413, "发布包为空或超过2GB")
			return
		}
		var stat syscall.Statfs_t
		if err := syscall.Statfs(a.StateDir, &stat); err != nil || uint64(stat.Bavail)*uint64(stat.Bsize) < uint64(MaxImage+1<<30) {
			errReply(507, "升级磁盘至少需要7GB可用空间")
			return
		}
		id := newID()
		dir, _ := packageDir(a.StateDir, id)
		if err := os.Mkdir(dir, 0700); err != nil {
			errReply(500, "无法创建发布暂存目录")
			return
		}
		keep := false
		defer func() {
			if !keep {
				os.RemoveAll(dir)
			}
		}()
		body := http.MaxBytesReader(w, r.Body, MaxUpload)
		manifest, err := Extract(body, dir, a.PublicKey)
		if err != nil {
			errReply(400, err.Error())
			return
		}
		rel := Release{id, manifest}
		if err := atomicJSON(filepath.Join(dir, "release.json"), rel); err != nil {
			errReply(500, "发布包记录保存失败")
			return
		}
		keep = true
		reply(201, rel)
	case r.Method == "POST" && r.URL.Path == "/install":
		if !a.mu.TryLock() {
			errReply(409, "另一个升级任务正在执行")
			return
		}
		owned := true
		defer func() {
			if owned {
				a.mu.Unlock()
			}
		}()
		if t := a.readTask(); t != nil && (active(t.Phase) || t.Phase == "rollback_failed") {
			errReply(409, "存在未完成或需人工恢复的升级任务")
			return
		}
		var in struct {
			PackageID string `json:"package_id"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in) != nil {
			errReply(400, "请求无效")
			return
		}
		dir, err := packageDir(a.StateDir, in.PackageID)
		if err != nil {
			errReply(400, "发布包不存在")
			return
		}
		b, err := os.ReadFile(filepath.Join(dir, "release.json"))
		var rel Release
		if err != nil || json.Unmarshal(b, &rel) != nil || rel.ID != in.PackageID {
			errReply(404, "发布包不存在")
			return
		}
		current, err := a.health()
		if err != nil || !rel.CanInstall(current) {
			errReply(409, "当前版本不在此发布包的兼容升级范围，不能降级或重复安装")
			return
		}
		lock, err := a.lock()
		if err != nil {
			errReply(409, "其他发布正在执行，请稍后重试")
			return
		}
		t := &Task{ID: newID(), PackageID: rel.ID, Version: rel.Version, PreviousVersion: current, StartedAt: time.Now().UTC()}
		if err := a.save(t, "queued", "升级任务已创建"); err != nil {
			unlock(lock)
			errReply(500, "无法保存任务")
			return
		}
		owned = false
		reply(202, t)
		go a.install(t, rel, lock)
	default:
		errReply(404, "接口不存在")
	}
}
