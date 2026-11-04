package openplatform

import (
	"bytes"
	"gorm.io/gorm"
)

// Rewrap 在停服维护窗口使用；全部解密、重新加密和核对成功才提交。
func (s *Service) Rewrap() (int, error) {
	n := 0
	err := s.DB.Transaction(func(tx *gorm.DB) error {
		var credentials []Credential
		if e := tx.Find(&credentials).Error; e != nil {
			return e
		}
		for _, c := range credentials {
			raw, e := s.Decrypt(c.AppID, c.AccessKey, c.Secret)
			if e != nil {
				return e
			}
			encrypted, e := s.Encrypt(c.AppID, c.AccessKey, raw)
			if e != nil {
				return e
			}
			check, e := s.Decrypt(c.AppID, c.AccessKey, encrypted)
			if e != nil || !bytes.Equal(raw, check) {
				return ErrUnavailable
			}
			c.Secret = encrypted
			if e = tx.Model(&c).Select("secret").Updates(&c).Error; e != nil {
				return e
			}
			n++
		}
		var subs []Subscription
		if e := tx.Find(&subs).Error; e != nil {
			return e
		}
		for _, sub := range subs {
			for i, key := range sub.Keys {
				binding := "callback:" + sub.ID + ":" + key.ID
				raw, e := s.Decrypt(sub.AppID, binding, key.Secret)
				if e != nil {
					return e
				}
				encrypted, e := s.Encrypt(sub.AppID, binding, raw)
				if e != nil {
					return e
				}
				check, e := s.Decrypt(sub.AppID, binding, encrypted)
				if e != nil || !bytes.Equal(raw, check) {
					return ErrUnavailable
				}
				sub.Keys[i].Secret = encrypted
				n++
			}
			if e := tx.Model(&sub).Select("keys").Updates(&sub).Error; e != nil {
				return e
			}
		}
		return nil
	})
	return n, err
}
