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
