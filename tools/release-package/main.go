// 发布工具仅由运维离线执行；签名私钥不挂载至应用或升级API。
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/gowvp/owl/internal/upgrader"
)

func main() {
	syscall.Umask(0077)
	keyPath := flag.String("key", "", "Ed25519 PKCS8私钥PEM路径")
	initKey := flag.Bool("init-key", false, "初始化私钥和相邻.public.pem，已有文件则拒绝")
	image := flag.String("image", "", "已构建镜像 owl-v1-service:vX.Y.Z")
	from := flag.String("from", "", "支持的来源版本，逗号分隔，必须显式指定")
	replaces := flag.String("replaces-version", "", "仅版本编号调整时填写唯一来源版本，必须与-from一致")
	notes := flag.String("notes", "", "更新说明")
	out := flag.String("out", "", "输出 .owl-release.tar.gz")
	flag.Parse()
	if *keyPath == "" {
		log.Fatal("必须指定-key")
	}
	if *initKey {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		must(err)
		b, err := x509.MarshalPKCS8PrivateKey(priv)
		must(err)
		writeNew(*keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: b}))
		b, err = x509.MarshalPKIXPublicKey(pub)
		must(err)
		writeNew(*keyPath+".public.pem", pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: b}))
		fmt.Println("发布签名密钥已创建；请单独保存私钥，只向升级服务安装公钥")
		return
	}
	if *image == "" || *from == "" || *out == "" {
		log.Fatal("必须指定-image、-from、-out")
	}
	b, err := os.ReadFile(*keyPath)
	must(err)
	block, _ := pem.Decode(b)
	if block == nil {
		log.Fatal("私钥格式无效")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	must(err)
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		log.Fatal("私钥必须为Ed25519")
	}
	inspect, err := exec.Command("docker", "image", "inspect", *image, "--format", "{{.Id}}|{{.Architecture}}|{{.Os}}|{{index .Config.Labels \"org.opencontainers.image.version\"}}").Output()
	must(err)
	parts := strings.Split(strings.TrimSpace(string(inspect)), "|")
	if len(parts) != 4 {
		log.Fatal("镜像元数据不完整")
	}
	dir, err := os.MkdirTemp("", "owl-package-*")
	must(err)
	defer os.RemoveAll(dir)
	imagePath := filepath.Join(dir, "image.tar")
	must(exec.Command("docker", "save", "--output", imagePath, *image).Run())
	f, err := os.Open(imagePath)
	must(err)
	h := sha256.New()
	size, err := io.Copy(h, f)
	f.Close()
	must(err)
	m := upgrader.Manifest{Format: 1, Product: "owl-docker", Version: parts[3], Image: *image, ImageID: parts[0], Architecture: parts[1], OS: parts[2], ImageSHA256: hex.EncodeToString(h.Sum(nil)), ImageSize: size, FromVersions: strings.Split(*from, ","), Notes: *notes}
	m.ReplacesVersion = *replaces
	must(m.Validate())
	body, err := json.MarshalIndent(m, "", "  ")
	must(err)
	writeNew(filepath.Join(dir, "manifest.json"), body)
	writeNew(filepath.Join(dir, "manifest.sig"), ed25519.Sign(priv, body))
	_, err = upgrader.Verify(dir, priv.Public().(ed25519.PublicKey))
	must(err)
	output, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	must(err)
	success := false
	defer func() {
		if !success {
			os.Remove(*out)
		}
	}()
	z := gzip.NewWriter(output)
	tarWriter := tar.NewWriter(z)
	for _, name := range []string{"manifest.json", "manifest.sig", "image.tar"} {
		file, err := os.Open(filepath.Join(dir, name))
		must(err)
		info, err := file.Stat()
		must(err)
		must(tarWriter.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: info.Size(), Typeflag: tar.TypeReg}))
		_, err = io.Copy(tarWriter, file)
		file.Close()
		must(err)
	}
	must(tarWriter.Close())
	must(z.Close())
	must(output.Sync())
	must(output.Close())
	info, err := os.Stat(*out)
	must(err)
	if info.Size() > upgrader.MaxUpload {
		log.Fatal("压缩包超过2GB")
	}
	success = true
	f, err = os.Open(*out)
	must(err)
	defer f.Close()
	h = sha256.New()
	_, err = io.Copy(h, f)
	must(err)
	fmt.Printf("%s  %s\n", hex.EncodeToString(h.Sum(nil)), *out)
}
func must(err error) {
	if err != nil {
		panic(err)
	}
}
func writeNew(path string, b []byte) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(err)
	_, err = f.Write(b)
	must(err)
	must(f.Sync())
	must(f.Close())
}
