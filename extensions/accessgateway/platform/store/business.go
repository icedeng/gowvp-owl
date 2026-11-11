package store

import (
	"github.com/gowvp/owl/extensions/accessgateway/platform/core"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"time"
)

// CheckBusinessBinding 在创建会话的同一事务内锁定绑定，与迁移授权和重新注册互斥。
// 调用者先锁网关，再锁绑定；迁移创建遵循相同顺序。
func CheckBusinessBinding(tx *gorm.DB, subjectType, subjectID, pk string, revision int64) error {
	table := "access_gateway_bindings"
	if subjectType == "platform" {
		table = "access_gateway_platform_bindings"
	}
	var binding BindingFields
	if err := tx.Table(table).Clauses(clause.Locking{Strength: "UPDATE"}).Where("subject_id = ?", subjectID).Take(&binding).Error; err != nil {
		return err
	}
	if binding.RegisteredUntil != nil && !time.Now().Before(*binding.RegisteredUntil) {
		return core.Fail(503, "SUBJECT_UNREGISTERED", "对象注册已失效")
	}
	if binding.GatewayID == nil || *binding.GatewayID != pk || binding.BindingRevision != revision {
		return core.Conflict("入口绑定已改变，请重新发起业务")
	}
	var active int64
	if err := tx.Table("access_gateway_migration_items AS i").Joins("JOIN access_gateway_migrations AS m ON m.id = i.migration_id").Where("i.subject_type = ? AND i.subject_id = ? AND i.active_subject_key IS NOT NULL AND m.expires_at > ? AND m.state IN ?", subjectType, subjectID, time.Now().UTC(), []string{"draining", "waiting_register"}).Count(&active).Error; err != nil {
		return err
	}
	if active > 0 {
		return core.Fail(409, "MIGRATION_DRAINING", "对象正在迁移，禁止创建新业务")
	}
	return nil
}
