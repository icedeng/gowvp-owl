package api

import (
	"testing"

	"github.com/gowvp/owl/internal/conf"
)

func TestNewNotifierWithoutTargetsReturnsNilDispatcher(t *testing.T) {
	dispatcher, cleanup := NewNotifier(&conf.Bootstrap{})
	defer cleanup()
	if dispatcher != nil {
		t.Fatalf("dispatcher = %#v, want nil", dispatcher)
	}
}
