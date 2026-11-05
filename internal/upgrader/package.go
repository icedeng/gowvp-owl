// Package upgrader 实现仅限签名应用镜像的宿主机升级，不执行发布包中的脚本。
package upgrader

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const MaxUpload int64 = 2 << 30
const MaxImage int64 = 6 << 30

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[a-zA-Z0-9][a-zA-Z0-9.-]{0,60})?$`)
var idPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)
var digestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Manifest struct {
	Format          int      `json:"format"`
	Product         string   `json:"product"`
	Version         string   `json:"version"`
	Architecture    string   `json:"architecture"`
	OS              string   `json:"os"`
	Image           string   `json:"image"`
	ImageID         string   `json:"image_id"`
	ImageSHA256     string   `json:"image_sha256"`
	ImageSize       int64    `json:"image_size"`
	FromVersions    []string `json:"from_versions"`
	ReplacesVersion string   `json:"replaces_version,omitempty"`
	Migration       bool     `json:"migration"`
	Notes           string   `json:"notes"`
}

type Release struct {
	ID string `json:"id"`
	Manifest
}

func ReadPublicKey(path string) (ed25519.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("无效的发布公钥")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := key.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("发布公钥必须为 Ed25519")
	}
	return pub, nil
}

func (m Manifest) Validate() error {
	if m.Format != 1 || m.Product != "owl-docker" || m.OS != "linux" || m.Architecture != "amd64" || m.Migration {
		return errors.New("发布包类型、架构不匹配，或包含不支持的数据库迁移")
	}
	if !versionPattern.MatchString(m.Version) || m.Image != "owl-v1-service:"+m.Version || !strings.HasPrefix(m.ImageID, "sha256:") || !digestPattern.MatchString(strings.TrimPrefix(m.ImageID, "sha256:")) || !digestPattern.MatchString(m.ImageSHA256) || m.ImageSize <= 0 || m.ImageSize > MaxImage || len(m.Notes) > 16000 || len(m.FromVersions) == 0 || len(m.FromVersions) > 100 {
		return errors.New("发布清单字段无效")
	}
	for _, v := range m.FromVersions {
		if !versionPattern.MatchString(v) {
			return errors.New("兼容版本无效")
		}
	}
	if m.ReplacesVersion != "" && (!versionPattern.MatchString(m.ReplacesVersion) || len(m.FromVersions) != 1 || m.FromVersions[0] != m.ReplacesVersion || m.Version == m.ReplacesVersion) {
		return errors.New("版本编号调整必须显式绑定唯一来源版本，且目标编号必须不同")
	}
	return nil
}

// CanInstall 普通升级禁止降级与同版本覆盖；编号调整须有显式签名声明。
func (m Manifest) CanInstall(current string) bool {
	if !versionPattern.MatchString(current) || !versionPattern.MatchString(m.Version) {
		return false
	}
	allowed := false
	for _, v := range m.FromVersions {
		allowed = allowed || current == v
	}
	if !allowed {
		return false
	}
	// 编号体系调整由签名清单显式授权，仅匹配指定的唯一来源，不放宽后续普通升级。
	if m.ReplacesVersion == current && len(m.FromVersions) == 1 && m.Version != current {
		return true
	}
	parts := func(v string) []int {
		v = strings.SplitN(strings.TrimPrefix(v, "v"), "-", 2)[0]
		x := strings.Split(v, ".")
		result := make([]int, 3)
		for i := range result {
			result[i], _ = strconv.Atoi(x[i])
		}
		return result
	}
	a, b := parts(m.Version), parts(current)
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// Extract 仅允许三个普通文件，限制解压量；不信任文件名、符号链接或包内脚本。
func Extract(reader io.Reader, dir string, pub ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	z, err := gzip.NewReader(reader)
	if err != nil {
		return m, errors.New("无法读取升级包，请上传 .owl-release.tar.gz")
	}
	defer z.Close()
	tr := tar.NewReader(z)
	seen := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return m, err
		}
		limit := int64(64 << 10)
		if h.Name == "image.tar" {
			limit = MaxImage
		}
		if (h.Name != "manifest.json" && h.Name != "manifest.sig" && h.Name != "image.tar") || seen[h.Name] || h.Typeflag != tar.TypeReg || h.Size <= 0 || h.Size > limit {
			return m, errors.New("发布包包含非法、重复或超限文件")
		}
		seen[h.Name] = true
		f, err := os.OpenFile(filepath.Join(dir, h.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return m, err
		}
		_, copyErr := io.CopyN(f, tr, h.Size)
		syncErr := f.Sync()
		closeErr := f.Close()
		if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
			return m, err
		}
	}
	// 读取gzip校验尾部，拒绝被截断的压缩流。
	if _, err := io.Copy(io.Discard, io.LimitReader(z, 1<<20)); err != nil {
		return m, err
	}
	if len(seen) != 3 {
		return m, errors.New("发布包缺少清单、签名或镜像")
	}
	return Verify(dir, pub)
}

func Verify(dir string, pub ed25519.PublicKey) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	sig, err := os.ReadFile(filepath.Join(dir, "manifest.sig"))
	if err != nil {
		return m, err
	}
	if len(pub) != ed25519.PublicKeySize || !ed25519.Verify(pub, b, sig) {
		return m, errors.New("发布签名无效，请使用本平台签发的升级包")
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return m, err
	}
	if err := m.Validate(); err != nil {
		return m, err
	}
	f, err := os.Open(filepath.Join(dir, "image.tar"))
	if err != nil {
		return m, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, MaxImage+1))
	if err != nil {
		return m, err
	}
	if n != m.ImageSize || hex.EncodeToString(h.Sum(nil)) != m.ImageSHA256 {
		return m, errors.New("镜像大小或 SHA256 不匹配")
	}
	if err := verifyImageArchive(dir, m); err != nil {
		return m, err
	}
	return m, nil
}

// 禁止签名包借docker load覆盖额外镜像标签；要求只有目标应用镜像。
func verifyImageArchive(dir string, m Manifest) error {
	f, err := os.Open(filepath.Join(dir, "image.tar"))
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var manifests []struct {
		Config   string
		RepoTags []string
	}
	seenManifest := false
	configHashes := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return errors.New("镜像归档无效")
		}
		if path.IsAbs(h.Name) || path.Clean(h.Name) == ".." || strings.HasPrefix(path.Clean(h.Name), "../") || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) {
			return errors.New("镜像归档包含不支持的文件")
		}
		if h.Name == "manifest.json" {
			if seenManifest || h.Size > 1<<20 {
				return errors.New("镜像归档清单无效")
			}
			seenManifest = true
			if json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&manifests) != nil {
				return errors.New("无法解析镜像清单")
			}
		}
		// Docker 29 的镜像ID可能是OCI清单摘要，与配置文件摘要不同。
		// 校验配置文件自身摘要；镜像ID在docker load后、切换应用前另行严格复核。
		nameDigest := strings.TrimSuffix(path.Base(h.Name), ".json")
		if h.Typeflag == tar.TypeReg && h.Size <= 1<<20 && digestPattern.MatchString(nameDigest) && (h.Name == nameDigest+".json" || h.Name == "blobs/sha256/"+nameDigest) {
			if len(configHashes) >= 512 {
				return errors.New("镜像归档小文件数量超限")
			}
			data, err := io.ReadAll(tr)
			if err != nil {
				return errors.New("镜像配置无法读取")
			}
			digest := sha256.Sum256(data)
			configHashes[h.Name] = hex.EncodeToString(digest[:])
		}
	}
	if len(manifests) != 1 || len(manifests[0].RepoTags) != 1 || manifests[0].RepoTags[0] != m.Image {
		return errors.New("镜像归档必须只包含目标应用标签")
	}
	config := manifests[0].Config
	digest := strings.TrimSuffix(path.Base(config), ".json")
	if !digestPattern.MatchString(digest) || (config != digest+".json" && config != "blobs/sha256/"+digest) || configHashes[config] != digest {
		return errors.New("镜像配置缺失或配置文件摘要不匹配")
	}
	return nil
}

func atomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(b, '\n'))
}

func atomicWrite(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func packageDir(root, id string) (string, error) {
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("发布包ID无效")
	}
	return filepath.Join(root, "catalog", id), nil
}
