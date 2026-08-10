package rpc

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/gowvp/owl/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type healthServer struct {
	protos.UnimplementedHealthServer
}

func (healthServer) Check(context.Context, *protos.HealthCheckRequest) (*protos.HealthCheckResponse, error) {
	return &protos.HealthCheckResponse{Status: protos.HealthCheckResponse_SERVING}, nil
}

func TestHealthCheck(t *testing.T) {
	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer()
	protos.RegisterHealthServer(server, healthServer{})
	go func() {
		_ = server.Serve(listener)
	}()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient(
		"passthrough:///bufnet",
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
			return listener.Dial()
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cli := protos.NewHealthClient(conn)
	resp, err := cli.Check(ctx, &protos.HealthCheckRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetStatus() != protos.HealthCheckResponse_SERVING {
		t.Fatalf("unexpected health status: %s", resp.GetStatus())
	}
}
