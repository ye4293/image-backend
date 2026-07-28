package generation_test

import (
	"testing"

	"image-backend/internal/database"
	"image-backend/internal/generation"
	"image-backend/internal/model"
)

// I2：注册成 nil 与没注册一样致命。返回 (nil, nil) 的话调用方会 panic，而那时行已
// 经建了、次数已经扣了。
func TestRegistryRejectsNilAdapter(t *testing.T) {
	r := generation.Registry{"flux": nil}
	a, err := r.Get("flux")
	if err == nil {
		t.Fatal("注册成 nil 应当报错，而不是返回 (nil, nil)")
	}
	if a != nil {
		t.Fatalf("出错时不该返回 adapter: %v", a)
	}
}

func TestRegistryGetMissingProvider(t *testing.T) {
	r := generation.Registry{}
	if _, err := r.Get("nope"); err == nil {
		t.Fatal("未注册的 provider 应当报错")
	}
}

// I2：provider 字段打错一个字母，不该等第一个选中该模型的用户以 500 的形式替我们
// 发现。启动期就该喊出来。
func TestValidateProvidersReportsUnresolvable(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	// 播种数据里的 flux-2-max 用 provider "flux"，这里再加一行拼错的。
	typo := model.ImageModel{
		ID: "typo-model", DisplayName: "Typo", Provider: "flx",
		UpstreamModel: "flux-2-max", Credits: 1, Enabled: true,
	}
	if err := db.Create(&typo).Error; err != nil {
		t.Fatal(err)
	}

	reg := generation.Registry{"flux": generation.NewStubAdapter()}
	problems := generation.ValidateProviders(db, reg)
	if len(problems) != 1 {
		t.Fatalf("应当恰好报出 1 个问题: %v", problems)
	}

	// 禁用的行不算问题——运营下架一个模型不该让启动日志报警。
	if err := db.Model(&model.ImageModel{}).Where("id = ?", "typo-model").
		Update("enabled", false).Error; err != nil {
		t.Fatal(err)
	}
	if problems := generation.ValidateProviders(db, reg); len(problems) != 0 {
		t.Fatalf("禁用的行不该报问题: %v", problems)
	}
}

func TestValidateProvidersPassesWhenAllResolve(t *testing.T) {
	db, err := database.Open("")
	if err != nil {
		t.Fatal(err)
	}
	reg := generation.Registry{"flux": generation.NewStubAdapter()}
	if problems := generation.ValidateProviders(db, reg); len(problems) != 0 {
		t.Fatalf("播种数据应当全部可解析: %v", problems)
	}
}

// Adapter 接口的实现者必须能被 Registry 装下——编译期钉住，避免签名漂移。
var _ generation.Adapter = (*generation.StubAdapter)(nil)
var _ generation.Adapter = (*generation.FluxAdapter)(nil)
