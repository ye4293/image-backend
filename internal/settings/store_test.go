package settings

import (
	"encoding/base64"
	"testing"

	"gorm.io/gorm"

	"image-backend/internal/database"
	"image-backend/internal/model"
)

func newStore(t *testing.T) (*Store, *gorm.DB) {
	t.Helper()
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	key, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return NewStore(db, key), db
}

func TestStoreSetGetRoundTrip(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Set("r2Bucket", "images"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	all, err := s.All()
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	if all["r2Bucket"] != "images" {
		t.Errorf("读回不一致: %q", all["r2Bucket"])
	}
}

func TestStoreEncryptsSecretsAtRest(t *testing.T) {
	// 这条是这一层存在的理由：明文进库就会进备份。
	s, db := newStore(t)
	if err := s.Set("fluxApiKey", "sk-plaintext-must-not-appear"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var row model.AppSetting
	if err := db.Where("key = ?", "fluxApiKey").First(&row).Error; err != nil {
		t.Fatalf("读行: %v", err)
	}
	if !row.Encrypted {
		t.Error("secret 项的 Encrypted 必须是 true")
	}
	if row.Value == "sk-plaintext-must-not-appear" {
		t.Fatal("库里存的是明文——加密没有生效")
	}
	// 但读出来必须还是明文
	all, _ := s.All()
	if all["fluxApiKey"] != "sk-plaintext-must-not-appear" {
		t.Errorf("解密后不一致: %q", all["fluxApiKey"])
	}
}

func TestStoreNonSecretStoredPlain(t *testing.T) {
	// 非 secret 项不加密：加密它们只会让排查配置问题时必须写代码才能看值。
	s, db := newStore(t)
	if err := s.Set("r2Bucket", "images"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	var row model.AppSetting
	db.Where("key = ?", "r2Bucket").First(&row)
	if row.Encrypted || row.Value != "images" {
		t.Errorf("非 secret 应当明文存：encrypted=%v value=%q", row.Encrypted, row.Value)
	}
}

func TestStoreRejectsUnknownKey(t *testing.T) {
	s, _ := newStore(t)
	if err := s.Set("nope", "x"); err == nil {
		t.Error("未知 key 必须被拒绝")
	}
}

func TestStoreSetOverwrites(t *testing.T) {
	s, _ := newStore(t)
	_ = s.Set("r2Bucket", "a")
	_ = s.Set("r2Bucket", "b")
	all, _ := s.All()
	if all["r2Bucket"] != "b" {
		t.Errorf("覆写失败: %q", all["r2Bucket"])
	}
}

func TestSeedFromEnvFillsEmptyTable(t *testing.T) {
	s, _ := newStore(t)
	env := map[string]string{
		"FLUX_API_KEY": "sk-from-env",
		"R2_BUCKET":    "bucket-from-env",
	}
	n, err := s.SeedFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if n != 2 {
		t.Errorf("播种了 %d 项，期望 2", n)
	}
	all, _ := s.All()
	if all["fluxApiKey"] != "sk-from-env" || all["r2Bucket"] != "bucket-from-env" {
		t.Errorf("播种结果不对: %+v", all)
	}
}

func TestSeedFromEnvDoesNotOverwriteExisting(t *testing.T) {
	// **最重要的一条。** 覆盖已有值会让运营在后台改过的配置在每次容器重启后被
	// env 里的旧值悄悄改回去——而日志里什么都看不出来。
	s, _ := newStore(t)
	if err := s.Set("r2Bucket", "changed-in-admin"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	env := map[string]string{"R2_BUCKET": "stale-value-from-env"}
	if _, err := s.SeedFromEnv(func(k string) string { return env[k] }); err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	all, _ := s.All()
	if all["r2Bucket"] != "changed-in-admin" {
		t.Fatalf("播种覆盖了后台改过的值: %q", all["r2Bucket"])
	}
}

func TestSeedFromEnvSkipsEmptyEnv(t *testing.T) {
	s, _ := newStore(t)
	n, err := s.SeedFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("SeedFromEnv: %v", err)
	}
	if n != 0 {
		t.Errorf("env 全空时不该播种任何项，播了 %d", n)
	}
}

func TestAllDecryptFailureIsReported(t *testing.T) {
	// 换密钥后旧密文解不开。必须报错而不是静默给空值——静默的话表现是"上游 key
	// 突然没配"，排查方向完全错。
	s, db := newStore(t)
	if err := s.Set("fluxApiKey", "sk-x"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	other, _ := ParseKey(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	s2 := NewStore(db, other)
	if _, err := s2.All(); err == nil {
		t.Fatal("用错密钥读取必须报错")
	}
}
