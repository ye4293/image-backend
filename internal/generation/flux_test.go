package generation

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFluxSubmitReturnsImageFromPollingURL(t *testing.T) {
	var gotPath, gotKey, gotAuth string
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-key")
		gotAuth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cost":7,"id":"abc123","input_mp":0,"output_mp":1,
			"polling_url":"https://cdn.example/img.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "test-key", "flux-2-max")
	seed := 42
	res, err := a.Generate(context.Background(), GenerateRequest{
		Prompt: "a cat", Width: 1024, Height: 1024, Seed: &seed,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	if gotPath != "/flux/v1/flux-2-max" {
		t.Fatalf("提交路径错误: %s", gotPath)
	}
	// 提交端点用 x-key，**不是** Authorization。这是上游的不一致，实测所得。
	if gotKey != "test-key" {
		t.Fatalf("提交应当用 x-key 头: got %q", gotKey)
	}
	if gotAuth != "" {
		t.Fatalf("提交不该带 Authorization: got %q", gotAuth)
	}
	if gotBody["prompt"] != "a cat" || gotBody["width"] != float64(1024) {
		t.Fatalf("请求体错误: %+v", gotBody)
	}
	if gotBody["seed"] != float64(42) {
		t.Fatalf("seed 未透传: %+v", gotBody)
	}

	// polling_url 装的是最终图片 URL，不是轮询地址。
	if res.ImageURL != "https://cdn.example/img.png" {
		t.Fatalf("图片 URL 错误: %s", res.ImageURL)
	}
	if res.UpstreamID != "abc123" || res.UpstreamCost != 7 {
		t.Fatalf("上游元数据错误: %+v", res)
	}
}

func TestFluxOmitsSeedWhenNil(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"id":"x","polling_url":"https://cdn.example/a.png","status":"Ready"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	if _, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, present := gotBody["seed"]; present {
		t.Fatalf("Seed 为 nil 时不该出现在请求体里: %+v", gotBody)
	}
}

func TestFluxFallsBackToGetResultWhenNotReady(t *testing.T) {
	var getResultAuth, getResultKey, getResultID string
	calls := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/flux/v1/get_result" {
			getResultAuth = r.Header.Get("Authorization")
			getResultKey = r.Header.Get("x-key")
			getResultID = r.URL.Query().Get("id")
			_, _ = w.Write([]byte(`{"id":"abc","result":{"sample":"https://cdn.example/late.png"},"status":"Ready"}`))
			return
		}
		calls++
		// 提交返回未就绪，且没有给出图片 URL——必须走兜底查询。
		_, _ = w.Write([]byte(`{"id":"abc","status":"Pending"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "test-key", "flux-2-max")
	res, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
	if err != nil {
		t.Fatalf("应当通过兜底查询拿到结果: %v", err)
	}
	if res.ImageURL != "https://cdn.example/late.png" {
		t.Fatalf("兜底结果错误: %s", res.ImageURL)
	}
	if getResultID != "abc" {
		t.Fatalf("兜底查询未带 id: %q", getResultID)
	}
	// 兜底端点用 Authorization: Bearer，**不是** x-key。与提交端点相反。
	if getResultAuth != "Bearer test-key" {
		t.Fatalf("兜底应当用 Bearer: got %q", getResultAuth)
	}
	if getResultKey != "" {
		t.Fatalf("兜底不该带 x-key: got %q", getResultKey)
	}
}

func TestFluxUpstreamErrorIsWrapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"prompt rejected by safety filter"}`))
	}))
	defer srv.Close()

	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	_, err := a.Generate(context.Background(), GenerateRequest{Prompt: "p", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("上游 4xx 应当归一成 ErrUpstream: %v", err)
	}
}

func TestFluxHonorsContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a := NewFluxAdapter(srv.URL, "k", "flux-2-max")
	if _, err := a.Generate(ctx, GenerateRequest{Prompt: "p", Width: 1, Height: 1}); err == nil {
		t.Fatal("已取消的 ctx 应当立即返回错误")
	}
}
