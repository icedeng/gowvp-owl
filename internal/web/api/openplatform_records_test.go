package api

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gowvp/owl/internal/core/ipc"
	op "github.com/gowvp/owl/internal/core/openplatform"
	"github.com/gowvp/owl/pkg/gbs"
)

func TestOpenDeviceRecordResult(t *testing.T) {
	out, err := openDeviceRecordResult(&ipc.RecordQueryOutput{}, nil)
	if err != nil || !out.Complete || out.Data == nil || out.Incomplete != nil {
		t.Fatal(out, err)
	}
	out, err = openDeviceRecordResult(&ipc.RecordQueryOutput{TimeNum: 1}, fmt.Errorf("wrapped: %w", &gbs.RecordInfoIncompleteError{Received: 1, Expected: 3}))
	if err != nil || out.Complete || out.TimeNum != 1 || out.Incomplete.ReceivedCount != 1 || out.Incomplete.ExpectedCount != 3 {
		t.Fatal(out, err)
	}
	for _, tc := range []struct {
		err    error
		code   string
		status int
	}{
		{gbs.ErrRecordQueryTimeout, "DEVICE_RECORD_TIMEOUT", 504},
		{context.DeadlineExceeded, "DEVICE_RECORD_TIMEOUT", 504},
		{gbs.ErrDeviceOffline, "DEVICE_OFFLINE", 409},
		{gbs.ErrChannelNotExist, "NOT_FOUND", 404},
		{errors.New("MUST_NOT_LEAK internal device path"), "DEVICE_RECORD_QUERY_FAILED", 503},
	} {
		value, err := openDeviceRecordResult(nil, fmt.Errorf("wrapped: %w", tc.err))
		if value != nil || op.AsError(err).Code != tc.code || op.AsError(err).Status != tc.status {
			t.Fatal(value, err)
		}
	}
}
