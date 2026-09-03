//go:build wireinject

package app

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/google/wire"
	"github.com/gowvp/owl/internal/conf"
	"github.com/gowvp/owl/internal/data"
	"github.com/gowvp/owl/internal/web/api"
)

func wireApp(ctx context.Context, bc *conf.Bootstrap, log *slog.Logger) (http.Handler, func(), error) {
	panic(wire.Build(data.ProviderSet, api.ProviderVersionSet, api.ProviderSet))
}
