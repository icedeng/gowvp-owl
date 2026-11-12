package event

import (
	"context"
	"gorm.io/gorm"
)

type admissionKey struct{}

// WithAdmission 在事件写入事务内复核宿主归属，避免撤销/删除与报警接纳之间的竞态。
func WithAdmission(ctx context.Context, check func(*gorm.DB) error) context.Context {
	return context.WithValue(ctx, admissionKey{}, check)
}
func CheckAdmission(ctx context.Context, tx *gorm.DB) error {
	if check, ok := ctx.Value(admissionKey{}).(func(*gorm.DB) error); ok {
		return check(tx)
	}
	return nil
}
