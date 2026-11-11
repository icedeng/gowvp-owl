// Package credentials 管理 A 独立凭据文件，数据库和 HTTP 只保存/返回引用及元信息。
package credentials

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/transport"
)

const MaxPEMBytes = 512 << 10

type Files struct {
	mu   sync.Mutex
	root *os.Root
}
type Bundle struct {
	ID       string
	CARef    string
	CertRef  string
	KeyRef   string
	Metadata core.Certificate
}

func Open(path string) (*Files, error) {
	absolute, e := filepath.Abs(path)
	if e != nil {
		return nil, e
	}
	if e = os.MkdirAll(absolute, 0700); e != nil {
		return nil, e
	}
	info, e := os.Lstat(absolute)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("凭据目录必须是 0700 的真实目录")
	}
	real, e := filepath.EvalSymlinks(absolute)
	if e != nil {
		return nil, e
	}
	root, e := os.OpenRoot(real)
	if e != nil {
		return nil, e
	}
	return &Files{root: root}, nil
}
func (f *Files) Close() error { return f.root.Close() }
func Validate(ca, cert, key []byte) (*tls.Config, core.Certificate, error) {
	var metadata core.Certificate
	if len(ca) == 0 || len(cert) == 0 || len(key) == 0 || len(ca) > MaxPEMBytes || len(cert) > MaxPEMBytes || len(key) > MaxPEMBytes {
		return nil, metadata, fmt.Errorf("每份 PEM 文件必须为 1—512 KiB")
	}
	remainder := ca
	fingerprints := []string{}
	now := time.Now()
	for len(bytes.TrimSpace(remainder)) > 0 {
		block, rest := pem.Decode(remainder)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, metadata, fmt.Errorf("CA 包只能包含证书")
		}
		certificate, e := x509.ParseCertificate(block.Bytes)
		if e != nil || !certificate.IsCA || certificate.KeyUsage&x509.KeyUsageCertSign == 0 || now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
			return nil, metadata, fmt.Errorf("CA 证书无效、用途错误或已过期")
		}
		sum := sha256.Sum256(block.Bytes)
		fingerprints = append(fingerprints, hex.EncodeToString(sum[:]))
		if len(fingerprints) > 32 {
			return nil, metadata, fmt.Errorf("CA 包最多包含 32 张证书")
		}
		remainder = rest
	}
	cfg, e := transport.ClientTLS(ca, cert, key)
	if e != nil {
		return nil, metadata, fmt.Errorf("客户端证书链、用途或私钥不匹配")
	}
	leaf := cfg.Certificates[0].Leaf
	explicit := false
	for _, usage := range leaf.ExtKeyUsage {
		if usage == x509.ExtKeyUsageClientAuth {
			explicit = true
		}
	}
	if !explicit || leaf.IsCA || strings.TrimSpace(leaf.Subject.CommonName) == "" {
		return nil, metadata, fmt.Errorf("客户端证书须具有 clientAuth 用途和明确 CN")
	}
	hash := sha256.Sum256(leaf.Raw)
	sort.Strings(fingerprints)
	caHash := sha256.Sum256([]byte(strings.Join(fingerprints, "\n")))
	caFingerprint := hex.EncodeToString(caHash[:])
	if len(fingerprints) == 1 {
		caFingerprint = fingerprints[0]
	}
	metadata = core.Certificate{Fingerprint: hex.EncodeToString(hash[:]), CAFingerprint: caFingerprint, NotBefore: leaf.NotBefore.UTC(), NotAfter: leaf.NotAfter.UTC(), Status: "staged"}
	return cfg, metadata, nil
}
func (f *Files) Import(ca, cert, key []byte) (Bundle, error) {
	_, metadata, e := Validate(ca, cert, key)
	if e != nil {
		return Bundle{}, e
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.NewString()
	bundle := Bundle{ID: id, CARef: id + ".ca.pem", CertRef: id + ".cert.pem", KeyRef: id + ".key.pem", Metadata: metadata}
	bundle.Metadata.ID = id
	written := []string{}
	files := []struct {
		name string
		data []byte
	}{{bundle.CARef, ca}, {bundle.CertRef, cert}, {bundle.KeyRef, key}}
	for _, item := range files {
		if e = f.write(item.name, item.data); e != nil {
			for _, name := range written {
				f.root.Remove(name)
			}
			return Bundle{}, e
		}
		written = append(written, item.name)
	}
	return bundle, nil
}
func (f *Files) write(name string, data []byte) error {
	temp := ".import-" + uuid.NewString()
	file, e := f.root.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return fmt.Errorf("凭据文件创建失败")
	}
	defer f.root.Remove(temp)
	if _, e = file.Write(data); e == nil {
		e = file.Sync()
	}
	closeErr := file.Close()
	if e != nil || closeErr != nil {
		return fmt.Errorf("凭据文件保存失败")
	}
	if e = f.root.Rename(temp, name); e != nil {
		return fmt.Errorf("凭据文件提交失败")
	}
	dir, e := f.root.Open(".")
	if e != nil {
		return fmt.Errorf("凭据目录同步失败")
	}
	defer dir.Close()
	if e = dir.Sync(); e != nil {
		return fmt.Errorf("凭据目录同步失败")
	}
	return nil
}
func validRef(ref string) bool {
	for _, suffix := range []string{".ca.pem", ".cert.pem", ".key.pem"} {
		if strings.HasSuffix(ref, suffix) && len(ref) == 36+len(suffix) {
			_, e := uuid.Parse(ref[:36])
			return e == nil
		}
	}
	return false
}
func (f *Files) read(ref string) ([]byte, error) {
	if !validRef(ref) {
		return nil, fmt.Errorf("凭据引用无效")
	}
	info, e := f.root.Lstat(ref)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("凭据文件缺失或权限不安全")
	}
	file, e := f.root.Open(ref)
	if e != nil {
		return nil, fmt.Errorf("凭据文件无法读取")
	}
	defer file.Close()
	data, e := io.ReadAll(io.LimitReader(file, MaxPEMBytes+1))
	if e != nil || len(data) > MaxPEMBytes {
		return nil, fmt.Errorf("凭据文件损坏或超限")
	}
	return data, nil
}
func (f *Files) Load(caRef, certRef, keyRef string) (*tls.Config, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ca, e := f.read(caRef)
	if e != nil {
		return nil, e
	}
	cert, e := f.read(certRef)
	if e != nil {
		return nil, e
	}
	key, e := f.read(keyRef)
	if e != nil {
		return nil, e
	}
	defer clear(key)
	cfg, _, e := Validate(ca, cert, key)
	return cfg, e
}

// Remove 只由已经确认无数据库/恢复操作引用的调用方执行。
func (f *Files) Remove(bundle Bundle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, ref := range []string{bundle.CARef, bundle.CertRef, bundle.KeyRef} {
		if !validRef(ref) {
			return fmt.Errorf("凭据引用无效")
		}
		if e := f.root.Remove(ref); e != nil && !os.IsNotExist(e) {
			return fmt.Errorf("凭据清理失败")
		}
	}
	return nil
}
