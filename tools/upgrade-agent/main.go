package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gowvp/owl/internal/upgrader"
)

func main() {
	root := flag.String("root", "/www2/gowvp-v1", "部署目录")
	flag.Parse()
	syscall.Umask(0077)
	state := filepath.Join(*root, "upgrader")
	if err := os.MkdirAll(state, 0700); err != nil {
		log.Fatal(err)
	}
	key, err := upgrader.ReadPublicKey(filepath.Join(state, "release-public.pem"))
	if err != nil {
		log.Fatal(err)
	}
	a := &upgrader.Agent{Root: *root, StateDir: state, LockPath: "/run/lock/owl-v1-release.lock", PublicKey: key, HealthURL: "http://127.0.0.1:15123/health"}
	socket := filepath.Join(state, "socket", "agent.sock")
	if err := os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		log.Fatal(err)
	}
	// systemd单实例；禁止第二个进程误删正在使用的socket。
	instance, err := os.OpenFile(filepath.Join(state, "agent.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		log.Fatal(err)
	}
	if err := syscall.Flock(int(instance.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		log.Fatal("升级服务已经运行")
	}
	defer instance.Close()
	if err := a.Init(); err != nil {
		log.Fatal(err)
	}
	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		log.Fatal(err)
	}
	s := &http.Server{Handler: a, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Minute, WriteTimeout: 20 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	log.Fatal(s.Serve(l))
}
