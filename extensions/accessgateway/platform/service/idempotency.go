package service

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
)

func requestDigest(m core.Mutation, request any) (string, error) {
	return digest(struct {
		Request  any
		Revision *int64
	}{request, m.ExpectedRevision})
}
func replayResult(op store.Operation, id string) (core.Result, error) {
	if op.Status == "succeeded" && op.RequestSnapshot == "{}" {
		var result core.Result
		decoder := json.NewDecoder(strings.NewReader(op.Result))
		decoder.UseNumber()
		e := decoder.Decode(&result)
		return result, e
	}
	return core.Result{Status: 202, Body: operation(op, id)}, nil
}
func (s *Service) replay(ctx context.Context, m core.Mutation, id string, request any) (*core.Result, error) {
	if _, e := uuid.Parse(m.Key); e != nil || m.Actor == "" {
		return nil, core.Fail(400, "INVALID_IDEMPOTENCY_KEY", "缺少合法幂等键或操作者")
	}
	key, _ := digest([]string{m.Actor, m.Route, id, m.Key})
	op, e := s.repo.OperationByKey(ctx, key)
	if store.IsNotFound(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	hash, e := requestDigest(m, request)
	if e != nil {
		return nil, e
	}
	if hash != op.RequestHash {
		return nil, core.Fail(409, "IDEMPOTENCY_CONFLICT", "同一幂等键已用于不同参数")
	}
	result, e := replayResult(op, id)
	return &result, e
}
