# 后台设置页 · 后端实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans。步骤用 `- [ ]` 勾选。

**Goal:** 上游 key、R2 五项、`APP_BASE_URL` 搬进数据库（secret 加密），后台改完立即生效、不重启。

**Architecture:** `internal/settings` 包分四层——白名单 `spec`、加解密 `crypto`、读写与播种 `store`、原子快照与客户端构造 `runtime`。`router.go` 的三个构造点改为从 `Runtime` 取。

**Tech Stack:** Go 1.25 / Gin / GORM / `crypto/aes` + `crypto/cipher`（标准库，AES-256-GCM）。

**前置：** 设计文档 `docs/superpowers/specs/2026-08-02-admin-settings-design.md`（提交 `3ca9b1a`）。HEAD `3ca9b1a`，`go test ./...` 全绿。

**配套：** 前端 `/admin/settings` 页面另开一份计划，**本计划 6 个任务全绿之后**再开始。

---

## 文件结构

| 文件 | 职责 |
|---|---|
| `internal/model/setting.go`（新） | `AppSetting` 表 |
| `internal/settings/spec.go`（新） | 配置项白名单 + 校验规则 |
| `internal/settings/crypto.go`（新） | AES-256-GCM 加解密 + 掩码 |
| `internal/settings/store.go`（新） | 读写、首次播种 |
| `internal/settings/runtime.go`（新） | 原子快照、Reload、构造 Registry/Storage |
| `internal/config/config.go`（改） | 加 `ConfigEncryptionKey` 与校验 |
| `internal/database/database.go`（改） | 迁移 `AppSetting` |
| `internal/handler/admin_settings.go`（新） | GET / PATCH |
| `internal/server/router.go`（改） | 三个构造点改走 Runtime |
| `cmd/server/main.go`（改） | 建 Runtime、播种、拦截降级 |

**为什么拆四个文件而不是一个 `settings.go`：** 加解密要能被单独测（不碰数据库），白名单要能被 handler 与 store 同时引用而不循环依赖，runtime 持有并发原语。塞一个文件里这三件事会互相纠缠。

---

## Task 1：`AppSetting` 表与迁移

**Files:** Create `internal/model/setting.go`；Modify `internal/database/database.go`；Test `internal/database/database_test.go`

- [ ] **Step 1：先写失败的测试** — 追加到 `internal/database/database_test.go`：

```go
func TestAppSettingTableMigrated(t *testing.T) {
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	m := db.Migrator()
	if !m.HasTable(&model.AppSetting{}) {
		t.Fatal("app_settings 表没有被迁移出来")
	}
	for _, col := range []string{"key", "value", "encrypted", "updated_at"} {
		if !m.HasColumn(&model.AppSetting{}, col) {
			t.Errorf("app_settings 缺列 %s", col)
		}
	}
}

func TestAppSettingEncryptedDefaultsFalse(t *testing.T) {
	// 默认值写错成 true 会让明文项被当成密文去解密，表现是启动时全部配置解不开。
	db, err := Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	s := model.AppSetting{Key: "k1", Value: "v1"}
	if err := db.Create(&s).Error; err != nil {
		t.Fatalf("落库: %v", err)
	}
	var got model.AppSetting
	if err := db.Where("key = ?", "k1").First(&got).Error; err != nil {
		t.Fatalf("读回: %v", err)
	}
	if got.Encrypted {
		t.Error("Encrypted 默认值应当是 false")
	}
}
```

- [ ] **Step 2：跑测试确认失败** — `go test ./internal/database/ -run TestAppSetting -v`。Expected: 编译失败，`model.AppSetting` 不存在。

- [ ] **Step 3：实现** — Create `internal/model/setting.go`：

```go
package model

import "time"

// AppSetting 是运营可在后台修改的配置项，key/value 一行一项。
//
// 选 key/value 而不是宽表：新增一项配置不用改表结构。代价是没有列级类型约束，
// 由 internal/settings 的白名单与写入校验补上——而那比数据库类型更能表达
// "R2 公开域名不能是 S3 API 域名"这类规则。
//
// **不是所有配置都在这里。** DATABASE_URL / JWT_SECRET / PORT 必须留在环境变量
// （管理员要登录才能改设置，而登录本身依赖它们），Stripe 的两个 secret 也刻意
// 留在环境变量（见设计文档 §3）。
type AppSetting struct {
	Key string `gorm:"primaryKey;size:64"`
	// Value 非 secret 项存明文；secret 项存 base64(nonce||ciphertext)。
	Value string `gorm:"type:text;not null"`
	// Encrypted 标记 Value 是否为密文。
	//
	// 显式存一列而不是靠 Key 推断：将来轮换加密方式时要能区分"这行还是旧格式"，
	// 靠 key 名推断会让迁移期无法判断。默认 false——写成 true 会让明文项被当成
	// 密文去解密，表现是启动时所有配置都解不开。
	Encrypted bool `gorm:"not null;default:false"`
	UpdatedAt time.Time
}
```

在 `internal/database/database.go` 的 `AutoMigrate` 列表里加 `&model.AppSetting{},`（放在 `&model.StripeEvent{}` 之后）。

- [ ] **Step 4：跑测试确认通过** — `go test ./internal/database/ -run TestAppSetting -v`。Expected: PASS ×2。

- [ ] **Step 5：全量** — `go test ./...`。Expected: 全绿。

- [ ] **Step 6：提交**

```bash
git add internal/model/setting.go internal/database/database.go internal/database/database_test.go
git commit -m "feat: app_settings 表——运营可改配置的 key/value 存储"
```

---

## Task 2：加解密与掩码

**Files:** Create `internal/settings/crypto.go`, `internal/settings/crypto_test.go`

- [ ] **Step 1：先写失败的测试** — Create `internal/settings/crypto_test.go`：

```go
package settings

import (
	"encoding/base64"
	"strings"
	"testing"
)

// testKey 是一把合法的 32 字节密钥（base64）。
func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := testKey(t)
	ct, err := Encrypt(k, "sk_live_secret_value")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if strings.Contains(ct, "sk_live_secret_value") {
		t.Fatal("密文里出现了明文——加密没有真的发生")
	}
	pt, err := Decrypt(k, ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if pt != "sk_live_secret_value" {
		t.Errorf("往返后不一致: got %q", pt)
	}
}

func TestEncryptUsesRandomNonce(t *testing.T) {
	// 同一明文两次加密必须得到不同密文。nonce 固定的话，相同的 key 在库里长得
	// 一模一样，攻击者能据此判断两个环境是否用了同一把上游 key。
	k := testKey(t)
	a, _ := Encrypt(k, "same")
	b, _ := Encrypt(k, "same")
	if a == b {
		t.Fatal("两次加密得到相同密文——nonce 没有随机化")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	// GCM 的完整性保证：改一个字节就应当解不开，而不是解出乱码。
	k := testKey(t)
	ct, _ := Encrypt(k, "value")
	raw, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("解 base64: %v", err)
	}
	raw[len(raw)-1] ^= 0x01
	if _, err := Decrypt(k, base64.StdEncoding.EncodeToString(raw)); err == nil {
		t.Fatal("密文被改动后必须解密失败")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	ct, _ := Encrypt(testKey(t), "value")
	other, err := ParseKey(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if _, err := Decrypt(other, ct); err == nil {
		t.Fatal("换一把密钥必须解密失败")
	}
}

func TestParseKeyRejectsWrongLength(t *testing.T) {
	// 16 字节是合法 AES 密钥长度，但我们固定用 AES-256，混进来会让不同环境
	// 用不同强度而无人察觉。
	for _, bad := range []string{
		base64.StdEncoding.EncodeToString([]byte("short")),
		base64.StdEncoding.EncodeToString([]byte("0123456789abcdef")), // 16 字节
		"这不是 base64",
		"",
	} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("非法密钥 %q 应当被拒绝", bad)
		}
	}
}

func TestMaskKeepsTailAndHidesBody(t *testing.T) {
	got := Mask("sk_live_abcdefgh1234")
	if strings.Contains(got, "abcdefgh") {
		t.Errorf("掩码里泄露了中间部分: %q", got)
	}
	if !strings.HasSuffix(got, "1234") {
		t.Errorf("掩码要保留末四位以便辨认是哪一把: %q", got)
	}
}

func TestMaskShortValueRevealsNothing(t *testing.T) {
	// 短值不能"保留末四位"，否则等于把整个值露出来。
	for _, s := range []string{"", "a", "abcd", "abcde"} {
		got := Mask(s)
		if s != "" && strings.Contains(got, s) {
			t.Errorf("短值 %q 的掩码泄露了原值: %q", s, got)
		}
	}
}
```

- [ ] **Step 2：跑测试确认失败** — `go test ./internal/settings/ -v`。Expected: 编译失败，`ParseKey`/`Encrypt`/`Decrypt`/`Mask` 都不存在。

- [ ] **Step 3：实现** — Create `internal/settings/crypto.go`：

```go
// Package settings 管理运营可在后台修改的配置项。
//
// secret 类配置在库里以密文存放：明文写库意味着它们会出现在 pg_dump 备份、
// 任何有库读权限的人手里、被拖库时，以及排查问题时随手 SELECT * 的终端历史里。
package settings

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// keyLen 固定 32 字节（AES-256）。
//
// 不接受 16/24：AES-128 也是合法的，但允许多种长度会让不同环境在无人察觉的
// 情况下用上不同强度。
const keyLen = 32

// ParseKey 解析 base64 编码的主密钥。
func ParseKey(b64 string) ([]byte, error) {
	if b64 == "" {
		return nil, errors.New("CONFIG_ENCRYPTION_KEY 为空")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("CONFIG_ENCRYPTION_KEY 不是合法 base64: %w", err)
	}
	if len(raw) != keyLen {
		return nil, fmt.Errorf(
			"CONFIG_ENCRYPTION_KEY 解码后是 %d 字节，必须是 %d 字节（AES-256）；"+
				"生成：openssl rand -base64 32", len(raw), keyLen)
	}
	return raw, nil
}

// Encrypt 返回 base64(nonce || ciphertext)。
func Encrypt(key []byte, plaintext string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce: %w", err)
	}
	// nonce 前置拼在密文前：GCM 解密需要它，而它不必保密。**每次加密都要新的
	// nonce**——固定 nonce 会让同一明文产生同一密文，于是能从库里看出两个环境
	// 是否用了同一把上游 key。
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

func Decrypt(key []byte, encoded string) (string, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文长度不足，无法取出 nonce")
	}
	nonce, body := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	// GCM 会一并校验完整性：密文被改过一个字节就在这里失败，而不是解出乱码。
	pt, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败（密钥不对或密文被改动）: %w", err)
	}
	return string(pt), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != keyLen {
		return nil, fmt.Errorf("密钥必须是 %d 字节", keyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("构造 AES: %w", err)
	}
	return cipher.NewGCM(block)
}

// Mask 把 secret 变成可以安全回传给前端的形式。
//
// 保留末四位是为了让管理员能辨认"是不是我刚填的那一把"，而不必把值读出来。
// **短值一律全遮**：不到 8 位时"保留末四位"等于把大半个值露出去。
func Mask(s string) string {
	const keep = 4
	if len(s) < keep*2 {
		if s == "" {
			return ""
		}
		return strings.Repeat("•", 8)
	}
	return strings.Repeat("•", 8) + s[len(s)-keep:]
}
```

- [ ] **Step 4：跑测试确认通过** — `go test ./internal/settings/ -v`。Expected: 7 个测试全 PASS。

- [ ] **Step 5：提交**

```bash
git add internal/settings/crypto.go internal/settings/crypto_test.go
git commit -m "feat: settings 加解密层——AES-256-GCM 与 secret 掩码"
```

---

## Task 3：配置项白名单与写入校验

**Files:** Create `internal/settings/spec.go`, `internal/settings/spec_test.go`

- [ ] **Step 1：先写失败的测试** — Create `internal/settings/spec_test.go`：

```go
package settings

import "testing"

func TestSpecsCoverExactlyTheIntendedKeys(t *testing.T) {
	// 白名单是这个包的安全边界：不在表里的 key 一律拒绝写入。多一项等于把一个
	// 没人校验的配置暴露给后台，少一项等于该项永远改不了。
	want := map[string]bool{
		"ezlinkaiBaseUrl":   false,
		"fluxApiKey":        true,
		"r2Endpoint":        false,
		"r2AccessKeyId":     true,
		"r2SecretAccessKey": true,
		"r2Bucket":          false,
		"r2PublicBaseUrl":   false,
		"appBaseUrl":        false,
	}
	if len(Specs) != len(want) {
		t.Fatalf("白名单有 %d 项，期望 %d 项", len(Specs), len(want))
	}
	for _, s := range Specs {
		secret, ok := want[s.Key]
		if !ok {
			t.Errorf("白名单里有意料之外的 key: %s", s.Key)
			continue
		}
		if s.Secret != secret {
			t.Errorf("%s 的 Secret 应当是 %v", s.Key, secret)
		}
	}
}

func TestLookupRejectsUnknownKey(t *testing.T) {
	// 静默接受未知 key 会让打错字的保存"成功"而什么都没生效。
	if _, ok := Lookup("nope"); ok {
		t.Error("未知 key 必须查不到")
	}
	if _, ok := Lookup("r2Bucket"); !ok {
		t.Error("已知 key 应当查得到")
	}
}

func TestValidateRejectsBadR2PublicBaseURL(t *testing.T) {
	// 与 config.ValidateStorage 同一套规则——那三种填法都会让每张图 401 或变成
	// 相对路径，而上传全部"成功"。
	for _, bad := range []string{
		"https://acct.r2.cloudflarestorage.com",
		"https://acct.r2.cloudflarestorage.com/",
		"https://acct.r2.cloudflarestorage.com/images",
		"img.example.com",
		"//img.example.com",
		"ftp://img.example.com",
	} {
		if err := Validate("r2PublicBaseUrl", bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
}

func TestValidateAllowsGoodR2PublicBaseURL(t *testing.T) {
	for _, good := range []string{
		"https://img.example.com",
		"https://img.example.com/",
		"https://pub-abc123.r2.dev",
		"", // 空表示清空，合法
	} {
		if err := Validate("r2PublicBaseUrl", good); err != nil {
			t.Errorf("%q 应当被接受，得到 %v", good, err)
		}
	}
}

func TestValidateRejectsNonHTTPAppBaseURL(t *testing.T) {
	for _, bad := range []string{"app.example.com", "ftp://x"} {
		if err := Validate("appBaseUrl", bad); err == nil {
			t.Errorf("%q 应当被拒绝", bad)
		}
	}
	if err := Validate("appBaseUrl", "https://app.example.com"); err != nil {
		t.Errorf("合法值被拒: %v", err)
	}
}

func TestValidateUnknownKeyIsAnError(t *testing.T) {
	if err := Validate("nope", "x"); err == nil {
		t.Error("未知 key 的校验必须报错，而不是默认放行")
	}
}
```

- [ ] **Step 2：跑测试确认失败** — `go test ./internal/settings/ -run 'TestSpec|TestLookup|TestValidate' -v`。Expected: 编译失败。

- [ ] **Step 3：实现** — Create `internal/settings/spec.go`：

```go
package settings

import (
	"fmt"
	"net/url"
	"strings"
)

// Spec 描述一个可在后台修改的配置项。
type Spec struct {
	Key string
	// Secret 为 true 时值在库里加密存放，且**永不**通过 API 回传明文。
	Secret bool
	// EnvVar 首次启动播种时从哪个环境变量取（见设计文档 §2.4）。
	EnvVar string
}

// Specs 是白名单，也是这个包的安全边界。
//
// **管理接口只接受这里列出的 key。** 静默接受未知 key 会让一次打错字的保存看
// 起来成功、实际什么都没生效——而配置类的静默失效最难排查。
var Specs = []Spec{
	{Key: "ezlinkaiBaseUrl", EnvVar: "EZLINKAI_BASE_URL"},
	{Key: "fluxApiKey", Secret: true, EnvVar: "FLUX_API_KEY"},
	{Key: "r2Endpoint", EnvVar: "R2_ENDPOINT"},
	{Key: "r2AccessKeyId", Secret: true, EnvVar: "R2_ACCESS_KEY_ID"},
	{Key: "r2SecretAccessKey", Secret: true, EnvVar: "R2_SECRET_ACCESS_KEY"},
	{Key: "r2Bucket", EnvVar: "R2_BUCKET"},
	{Key: "r2PublicBaseUrl", EnvVar: "R2_PUBLIC_BASE_URL"},
	{Key: "appBaseUrl", EnvVar: "APP_BASE_URL"},
}

func Lookup(key string) (Spec, bool) {
	for _, s := range Specs {
		if s.Key == key {
			return s, true
		}
	}
	return Spec{}, false
}

// Validate 在**写入之前**校验。
//
// 这是主防线：它在坏数据产生之前拦住。启动期的同类校验只降级为告警（见设计
// 文档 §2.5），因为那时拒绝启动等于让一次误操作把服务打死。
func Validate(key, value string) error {
	if _, ok := Lookup(key); !ok {
		return fmt.Errorf("未知配置项 %q", key)
	}
	// 空值一律合法，表示清空该项（secret 清空即退化成未配置）。
	if value == "" {
		return nil
	}
	switch key {
	case "r2PublicBaseUrl":
		u, err := url.Parse(value)
		if err != nil {
			return fmt.Errorf("r2PublicBaseUrl 解析失败：%w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf(
				"r2PublicBaseUrl 是 %q，必须以 http:// 或 https:// 开头——"+
					"否则拼出来的图片地址会被浏览器当成相对路径", value)
		}
		// 用后缀匹配而不是和 r2Endpoint 比字符串：带末尾斜杠、带路径、换个
		// account id 的粘贴都坏得一模一样，字符串相等一个都拦不住。
		// *.r2.dev 是 R2 正经的公开域名，不能拦。
		if strings.HasSuffix(u.Hostname(), ".r2.cloudflarestorage.com") {
			return fmt.Errorf(
				"r2PublicBaseUrl 是 %q，那是 S3 API 域名、不允许匿名读——"+
					"上传会成功但每张图在浏览器里都是 401；请填绑在桶上的自定义域"+
					"或 *.r2.dev 公开域名", value)
		}
	case "appBaseUrl", "r2Endpoint", "ezlinkaiBaseUrl":
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s 是 %q，必须是完整的 http(s) URL", key, value)
		}
	}
	return nil
}
```

- [ ] **Step 4：跑测试确认通过** — `go test ./internal/settings/ -v`。Expected: 全 PASS（含 Task 2 的 7 个）。

- [ ] **Step 5：提交**

```bash
git add internal/settings/spec.go internal/settings/spec_test.go
git commit -m "feat: settings 白名单与写入期校验"
```

---

## Task 4：Store——读写与首次播种

**Files:** Create `internal/settings/store.go`, `internal/settings/store_test.go`

- [ ] **Step 1：先写失败的测试** — Create `internal/settings/store_test.go`。测试用 `database.Open("")` 起内存库：

```go
package settings

import (
	"encoding/base64"
	"testing"

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
```

需要 import `gorm.io/gorm`。

- [ ] **Step 2：跑测试确认失败** — `go test ./internal/settings/ -run 'TestStore|TestSeed|TestAll' -v`。Expected: 编译失败。

- [ ] **Step 3：实现** — Create `internal/settings/store.go`：

```go
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
```

- [ ] **Step 4：跑测试确认通过** — `go test ./internal/settings/ -v`。Expected: 全 PASS。

- [ ] **Step 5：全量并提交**

```bash
go test ./...
git add internal/settings/store.go internal/settings/store_test.go
git commit -m "feat: settings Store——加密读写与首次从 env 播种"
```

---

## Task 5：Runtime——原子快照与热重载

**Files:** Create `internal/settings/runtime.go`, `internal/settings/runtime_test.go`；Modify `internal/config/config.go`

- [ ] **Step 1：`config.go` 加主密钥项**

加字段：

```go
	// ConfigEncryptionKey 加密 app_settings 里 secret 项的主密钥（base64 的 32 字节）。
	//
	// 这一项**不能**搬进后台——它正是用来解开后台里那些 secret 的。
	// 生成：openssl rand -base64 32
	ConfigEncryptionKey string
```

`Load()` 里 `ConfigEncryptionKey: getEnv("CONFIG_ENCRYPTION_KEY", "")`。

- [ ] **Step 2：先写失败的测试** — Create `internal/settings/runtime_test.go`：

```go
package settings

import (
	"context"
	"encoding/base64"
	"testing"

	"image-backend/internal/database"
	"image-backend/internal/generation"
)

func newRuntime(t *testing.T) (*Runtime, *Store) {
	t.Helper()
	db, err := database.Open("")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	key, _ := ParseKey(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	st := NewStore(db, key)
	rt, err := NewRuntime(st)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	return rt, st
}

func TestRuntimeFallsBackToStubWhenNoFluxKey(t *testing.T) {
	// 未配 key 时必须是 stub，而不是一个拿空 key 去打上游的 adapter——后者会让
	// 每次生成都以"上游认证失败"收场并扣掉次数。
	rt, _ := newRuntime(t)
	a, err := rt.Adapters().Get("flux")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if a == nil {
		t.Fatal("adapter 是 nil")
	}
	if rt.StorageEnabled() {
		t.Error("五项都没配，StorageEnabled 应当是 false")
	}
}

func TestRuntimeReloadPicksUpNewSettings(t *testing.T) {
	// 热重载的核心断言：改完配置**不重启**就生效。
	rt, st := newRuntime(t)
	before := rt.Snapshot().R2Bucket
	if before != "" {
		t.Fatalf("初始 bucket 应当为空，got %q", before)
	}
	for k, v := range map[string]string{
		"r2Endpoint":        "https://acct.r2.cloudflarestorage.com",
		"r2AccessKeyId":     "ak",
		"r2SecretAccessKey": "sk",
		"r2Bucket":          "images-v2",
		"r2PublicBaseUrl":   "https://img.example.com",
	} {
		if err := st.Set(k, v); err != nil {
			t.Fatalf("Set %s: %v", k, err)
		}
	}
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := rt.Snapshot().R2Bucket; got != "images-v2" {
		t.Errorf("Reload 后 bucket 应当是 images-v2, got %q", got)
	}
	if !rt.StorageEnabled() {
		t.Error("五项齐全后 StorageEnabled 应当是 true")
	}
}

func TestRuntimeSnapshotIsStableDuringReload(t *testing.T) {
	// 读方拿到的快照不能在使用中途被换掉。这里断言的是"拿到之后就不变"，
	// 也就是原子替换而非就地修改。
	rt, st := newRuntime(t)
	snap := rt.Snapshot()
	_ = st.Set("r2Bucket", "later")
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if snap.R2Bucket != "" {
		t.Error("已经取到的快照被就地改动了——必须是原子替换")
	}
}

func TestRuntimeAdaptersAlwaysWrappedInStoringAdapter(t *testing.T) {
	// 与 BuildAdapters 那条守卫同理：转存靠包这一层实现，而 stub 返回相对路径
	// 让"有没有包"在行为上看不出来。
	rt, _ := newRuntime(t)
	for name, a := range rt.Adapters() {
		if _, ok := a.(*generation.StoringAdapter); !ok {
			t.Errorf("provider %q 没有被 StoringAdapter 包住——转存会整个静默失效", name)
		}
	}
}

func TestRuntimeGenerateStillWorksAfterReload(t *testing.T) {
	// Reload 之后 Registry 必须仍然可用（不能出现悬空的 nil adapter）。
	rt, st := newRuntime(t)
	_ = st.Set("r2Bucket", "b")
	if err := rt.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	a, err := rt.Adapters().Get("flux")
	if err != nil {
		t.Fatalf("Reload 后 Get: %v", err)
	}
	if _, err := a.Generate(context.Background(), generation.GenerateRequest{
		Prompt: "quick x", Width: 1024, Height: 1024,
		UpstreamModel: "flux-2-max", GenerationID: "g1",
	}); err != nil {
		t.Fatalf("Reload 后生成失败: %v", err)
	}
}
```

- [ ] **Step 3：跑测试确认失败** — `go test ./internal/settings/ -run TestRuntime -v`。Expected: 编译失败。

- [ ] **Step 4：实现** — Create `internal/settings/runtime.go`：

```go
package settings

import (
	"fmt"
	"log"
	"sync/atomic"

	"image-backend/internal/generation"
	"image-backend/internal/storage"
)

// Snapshot 是某一时刻生效的配置值。
//
// 只读：Reload 会整体换掉一个新的 Snapshot 指针，绝不就地修改已发出去的那个。
type Snapshot struct {
	EZLinkAIBaseURL   string
	FluxAPIKey        string
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2PublicBaseURL   string
	AppBaseURL        string

	// adapters 与 storage 是按上面的值构造好的客户端，随快照一起替换。
	adapters generation.Registry
}

// StorageEnabled 五项齐全才算配置好（与 config.StorageEnabled 同一判断）。
func (s *Snapshot) StorageEnabled() bool {
	return s.R2Endpoint != "" && s.R2AccessKeyID != "" && s.R2SecretAccessKey != "" &&
		s.R2Bucket != "" && s.R2PublicBaseURL != ""
}

// Runtime 持有当前生效的配置与客户端，支持不重启热替换。
//
// 用 atomic.Pointer 而不是读写锁：读方是每一个请求（生成请求会持有数十秒），
// 加锁读会让写方在改配置时被长请求挡住，而无锁读没有这个问题。写极少发生，
// 每次重建全部客户端的成本可以忽略。
type Runtime struct {
	store *Store
	snap  atomic.Pointer[Snapshot]
}

func NewRuntime(store *Store) (*Runtime, error) {
	rt := &Runtime{store: store}
	if err := rt.Reload(); err != nil {
		return nil, err
	}
	return rt, nil
}

// Reload 从库里重读配置、重建客户端，然后原子替换快照。
func (rt *Runtime) Reload() error {
	vals, err := rt.store.All()
	if err != nil {
		return err
	}
	s := &Snapshot{
		EZLinkAIBaseURL:   valOr(vals, "ezlinkaiBaseUrl", "https://api.ezlinkai.com"),
		FluxAPIKey:        vals["fluxApiKey"],
		R2Endpoint:        vals["r2Endpoint"],
		R2AccessKeyID:     vals["r2AccessKeyId"],
		R2SecretAccessKey: vals["r2SecretAccessKey"],
		R2Bucket:          vals["r2Bucket"],
		R2PublicBaseURL:   vals["r2PublicBaseUrl"],
		AppBaseURL:        vals["appBaseUrl"],
	}
	s.adapters = buildAdapters(s)
	rt.snap.Store(s)
	return nil
}

func (rt *Runtime) Snapshot() *Snapshot          { return rt.snap.Load() }
func (rt *Runtime) Adapters() generation.Registry { return rt.snap.Load().adapters }
func (rt *Runtime) StorageEnabled() bool          { return rt.snap.Load().StorageEnabled() }
func (rt *Runtime) AppBaseURL() string            { return rt.snap.Load().AppBaseURL }

// buildAdapters 与原先 server.BuildAdapters 同构，只是配置来自快照而非 env。
//
// 每个 adapter 都被 StoringAdapter 包一层：新增 provider 自动获得转存，不依赖
// 谁记得加代码。
func buildAdapters(s *Snapshot) generation.Registry {
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFlux(s), buildStorage(s)),
	}
}

func buildFlux(s *Snapshot) generation.Adapter {
	if s.FluxAPIKey == "" {
		// 退化成 stub 而不是拿空 key 去打上游：后者会让每次生成都以"上游认证
		// 失败"收场，而次数已经扣了。
		log.Println("settings: 未配置 fluxApiKey，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	return generation.NewFluxAdapter(s.EZLinkAIBaseURL, s.FluxAPIKey)
}

func buildStorage(s *Snapshot) storage.Storage {
	if !s.StorageEnabled() {
		log.Println("settings: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		s.R2Endpoint, s.R2AccessKeyID, s.R2SecretAccessKey,
		s.R2Bucket, s.R2PublicBaseURL,
	)
}

func valOr(m map[string]string, key, def string) string {
	if v := m[key]; v != "" {
		return v
	}
	return def
}

// Validate 报告当前生效配置里的问题，供启动时打告警用。
//
// **返回问题列表而不是 error，也不让调用方 Fatal。** 库里的值可能是上一个管理员
// 改坏的，此时拒绝启动等于让一次误操作把服务打死，而正确的行为是带着告警起来、
// 让管理员能登录进去改回来（见设计文档 §2.5）。
func (rt *Runtime) Validate() []string {
	s := rt.Snapshot()
	var problems []string
	for _, kv := range []struct{ k, v string }{
		{"r2PublicBaseUrl", s.R2PublicBaseURL},
		{"appBaseUrl", s.AppBaseURL},
		{"r2Endpoint", s.R2Endpoint},
		{"ezlinkaiBaseUrl", s.EZLinkAIBaseURL},
	} {
		if err := Validate(kv.k, kv.v); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}
```

- [ ] **Step 5：跑测试确认通过** — `go test ./internal/settings/ ./internal/config/ -v`。Expected: 全 PASS。

- [ ] **Step 6：提交**

```bash
git add internal/settings/runtime.go internal/settings/runtime_test.go internal/config/config.go
git commit -m "feat: settings Runtime——原子快照与不重启热重载"
```

---

## Task 6：接线与管理接口

**Files:** Create `internal/handler/admin_settings.go`, `internal/server/admin_settings_test.go`；Modify `internal/server/router.go`, `internal/handler/generations.go`, `cmd/server/main.go`

- [ ] **Step 1：先写失败的测试** — Create `internal/server/admin_settings_test.go`。要覆盖：

1. `GET /api/v1/admin/settings` 非管理员 403
2. GET 返回非 secret 项的 `value`、secret 项只有 `configured` + `masked`
3. **GET 响应体里不含任何 secret 明文**（对每个 secret 项断言 `!strings.Contains(body, plaintext)`）
4. `PATCH` 未知 key → 400
5. `PATCH` 非法 `r2PublicBaseUrl`（S3 域名 / 缺 scheme）→ 400
6. `PATCH` 成功 → 200，且**同一进程内**下一次 `GET` 反映新值（热重载生效，不重启）
7. `PATCH` secret 传空串 → 该项 `configured:false`
8. `PATCH` 空 body / 没有任何已知 key → 400

- [ ] **Step 2：跑测试确认失败** — 路由未注册，全部 404。

- [ ] **Step 3：实现 handler** — Create `internal/handler/admin_settings.go`。要点：

- `AdminSettingsHandler{Store *settings.Store, Runtime *settings.Runtime}`
- GET：`Store.All()` 拿明文 → 按 `settings.Specs` 逐项组装；`Secret` 项只输出 `{"configured": v != "", "masked": settings.Mask(v)}`，**绝不输出 value**
- PATCH：`map[string]string` 绑定 → 逐项 `Store.Set`（它自带白名单与校验）→ 全部成功后 `Runtime.Reload()` → 返回与 GET 同形状
- PATCH 里任何一项校验失败就整体 400 且**不 Reload**，避免"改了一半"

- [ ] **Step 4：接线 router** — `NewRouter` 里建 Store 与 Runtime；`GenerationsHandler` 的 `Adapters` 字段改为 `func() generation.Registry`（或持有 Runtime），每次请求取当前快照；`billing.New` 的第二参改为 `rt.AppBaseURL()`。注册：

```go
	adminSettings := &handler.AdminSettingsHandler{Store: st, Runtime: rt}
	admin.GET("/settings", adminSettings.Get)
	admin.PATCH("/settings", adminSettings.Patch)
```

**注意 `NewRouterWithAdapters` 的测试注入口要保留**——现有测试依赖它注入自己的 stub。可以让它接受一个已构造的 Registry 并让 Runtime 在该模式下不接管 adapters。

- [ ] **Step 5：`cmd/server/main.go`**

- 解析 `CONFIG_ENCRYPTION_KEY`：失败**拒绝启动**（解不开的密文等于所有上游凭据失效，那时起来也没用且会以"上游认证失败"误导排查）
- `SeedFromEnv(os.Getenv)`，播种数 > 0 时打日志说明哪些项来自 env、且今后以库为准
- `rt.Validate()` 的每条问题打 `log.Printf` 告警，**不 Fatal**
- 原有 `cfg.ValidateStorage()` 的 Fatal 移除（改由后台校验 + 启动告警承担）

- [ ] **Step 6：跑全量并提交**

```bash
go test ./... && go vet ./...
git add internal/handler/admin_settings.go internal/server/admin_settings_test.go internal/server/router.go internal/handler/generations.go cmd/server/main.go
git commit -m "feat: 后台设置接口与接线——改完立即生效不重启"
```

---

## 后端完成检查

- [ ] `go test ./...` 全绿、`go vet ./...` 干净
- [ ] `.env.example` / `.env.prod.example` 加 `CONFIG_ENCRYPTION_KEY`，并注明 `FLUX_API_KEY` / `R2_*` / `APP_BASE_URL` **只在首次启动时被读取**（之后以后台为准）
- [ ] 手工：起服务 → `GET /admin/settings` 确认响应里没有任何 secret 明文
- [ ] 手工：`PATCH` 改 `r2Bucket` → **不重启** → 生成一张图 → 确认对象进了新桶
- [ ] 手工：把 `r2PublicBaseUrl` 改成 S3 API 域名 → 400 且带说明
- [ ] 手工：库里直接塞一个非法 `r2PublicBaseUrl`（模拟被改坏）→ 重启 → **启动成功**并打告警

**只有以上全绿之后**才开始前端 `/admin/settings` 页面的计划。
