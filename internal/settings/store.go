package settings

import (
	"fmt"

	"gorm.io/gorm"

	"image-backend/internal/model"
)

// Store 读写 app_settings 表，secret 项自动加解密。
type Store struct {
	db  *gorm.DB
	key []byte
}

func NewStore(db *gorm.DB, key []byte) *Store {
	return &Store{db: db, key: key}
}

// All 返回所有已配置项的**明文**值，key 为白名单里的 key。
//
// 解密失败会返回错误而不是跳过那一项：静默给空值的表现是"上游 key 突然没配"，
// 会把排查方向完全带偏（真实原因是 CONFIG_ENCRYPTION_KEY 换了或密文损坏）。
func (s *Store) All() (map[string]string, error) {
	var rows []model.AppSetting
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("读取设置: %w", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		if _, ok := Lookup(r.Key); !ok {
			// 库里有白名单外的 key：可能是降级留下的。忽略但不报错。
			continue
		}
		if !r.Encrypted {
			out[r.Key] = r.Value
			continue
		}
		pt, err := Decrypt(s.key, r.Value)
		if err != nil {
			return nil, fmt.Errorf("解密设置项 %s 失败（CONFIG_ENCRYPTION_KEY 是否变过？）: %w", r.Key, err)
		}
		out[r.Key] = pt
	}
	return out, nil
}

// Set 写入一项。未知 key 与校验失败都会被拒绝。
func (s *Store) Set(key, value string) error {
	spec, ok := Lookup(key)
	if !ok {
		return fmt.Errorf("未知配置项 %q", key)
	}
	if err := Validate(key, value); err != nil {
		return err
	}
	stored, encrypted := value, false
	if spec.Secret && value != "" {
		ct, err := Encrypt(s.key, value)
		if err != nil {
			return fmt.Errorf("加密设置项 %s: %w", key, err)
		}
		stored, encrypted = ct, true
	}
	row := model.AppSetting{Key: key, Value: stored, Encrypted: encrypted}
	// 按主键 upsert：Save 在主键存在时是 UPDATE，不存在时是 INSERT。
	if err := s.db.Save(&row).Error; err != nil {
		return fmt.Errorf("写入设置项 %s: %w", key, err)
	}
	return nil
}

// SeedFromEnv 在库里**还没有**某一项时，用环境变量的值把它播种进去，返回播种项数。
//
// getenv 作为参数传入而不是直接调 os.Getenv，只为让测试能注入。
//
// **绝不覆盖已有值。** 覆盖会让运营在后台改过的配置在每次容器重启后被 env 里的
// 旧值悄悄改回去，而日志里什么都看不出来。
func (s *Store) SeedFromEnv(getenv func(string) string) (int, error) {
	var existing []model.AppSetting
	if err := s.db.Find(&existing).Error; err != nil {
		return 0, fmt.Errorf("读取现有设置: %w", err)
	}
	have := make(map[string]bool, len(existing))
	for _, r := range existing {
		have[r.Key] = true
	}

	n := 0
	for _, spec := range Specs {
		if have[spec.Key] || spec.EnvVar == "" {
			continue
		}
		v := getenv(spec.EnvVar)
		if v == "" {
			continue
		}
		if err := s.Set(spec.Key, v); err != nil {
			return n, fmt.Errorf("播种 %s: %w", spec.Key, err)
		}
		n++
	}
	return n, nil
}
