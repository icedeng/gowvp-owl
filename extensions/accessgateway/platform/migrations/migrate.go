// Package migrations 挂接宿主版本迁移；任何失败均返回上层，禁止跳过启动。
package migrations

import (
	"fmt"
	"github.com/gowvp/owl/extensions/accessgateway/platform/store"
	"gorm.io/gorm"
	"gorm.io/gorm/schema"
	"sync"
)

func Apply(db *gorm.DB) error {
	if db.Dialector.Name() == "mysql" {
		if err := db.Set("gorm:table_options", "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin").AutoMigrate(store.Models()...); err != nil {
			return err
		}
		return Verify(db)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.AutoMigrate(store.Models()...); err != nil {
			return err
		}
		return Verify(tx)
	})
}
func Verify(db *gorm.DB) error {
	for _, model := range store.Models() {
		s, err := schema.Parse(model, &sync.Map{}, db.NamingStrategy)
		if err != nil {
			return err
		}
		m := db.Migrator()
		if !m.HasTable(model) {
			return fmt.Errorf("接入网关缺少数据表 %s", s.Table)
		}
		for _, field := range s.Fields {
			if field.DBName != "" && !m.HasColumn(model, field.DBName) {
				return fmt.Errorf("接入网关缺少字段 %s.%s", s.Table, field.DBName)
			}
		}
		for _, index := range s.ParseIndexes() {
			if !m.HasIndex(model, index.Name) {
				return fmt.Errorf("接入网关缺少索引 %s", index.Name)
			}
		}
	}
	if db.Dialector.Name() == "mysql" {
		var collation string
		if err := db.Raw("SELECT COLLATION_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA=DATABASE() AND TABLE_NAME=? AND COLUMN_NAME=?", "access_gateways", "stable_id").Scan(&collation).Error; err != nil {
			return err
		}
		if collation != "utf8mb4_bin" {
			return fmt.Errorf("access_gateways.stable_id 必须使用 utf8mb4_bin 精确比较")
		}
	}
	return nil
}
