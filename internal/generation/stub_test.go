package generation

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStubQuickSucceedsFast(t *testing.T) {
	s := NewStubAdapter()
	start := time.Now()
	res, err := s.Generate(context.Background(), GenerateRequest{Prompt: "quick cat", Width: 1024, Height: 1024})
	if err != nil {
		t.Fatalf("quick 应当成功: %v", err)
	}
	if res.ImageURL == "" {
		t.Fatal("应当返回占位图 URL")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("quick 应当很快返回，实际 %v", elapsed)
	}
}

func TestStubFailKeywordReturnsUpstreamError(t *testing.T) {
	s := NewStubAdapter()
	_, err := s.Generate(context.Background(), GenerateRequest{Prompt: "please fail", Width: 1024, Height: 1024})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("fail 关键词应当返回 ErrUpstream: %v", err)
	}
}

func TestStubKeywordPriority(t *testing.T) {
	// 与前端 M2 假数据一致：fail > slow > quick > 默认。变了会让端到端测试的
	// 预期悄悄失效。
	s := NewStubAdapter()
	_, err := s.Generate(context.Background(), GenerateRequest{Prompt: "quick fail", Width: 1, Height: 1})
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("fail 优先级应当高于 quick: %v", err)
	}
}

// slow > quick 此前没测——交换 stub.go 里那两个 case 仍然全绿，但会把 90 秒那条
// 端到端路径悄悄改成 200 毫秒。这里靠"400 毫秒内不该返回成功"来钉住。
func TestStubSlowBeatsQuick(t *testing.T) {
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := s.Generate(ctx, GenerateRequest{Prompt: "quick slow", Width: 1, Height: 1}); err == nil {
		t.Fatal("slow 优先级应当高于 quick：400 毫秒内不该返回成功")
	}
}

// stub 记下收到的请求，这样 server 层测试才能发现 handler 把画幅译错或漏传 Seed。
// 忽略入参的 stub 会让接口层的这类 bug 完全隐形。
func TestStubRecordsLastRequest(t *testing.T) {
	s := NewStubAdapter()
	seed := 7
	if _, err := s.Generate(context.Background(), GenerateRequest{
		Prompt: "quick cat", Width: 1344, Height: 768, Seed: &seed, UpstreamModel: "flux-2-max",
	}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	got, ok := s.LastRequest()
	if !ok {
		t.Fatal("应当记下收到的请求")
	}
	if got.Width != 1344 || got.Height != 768 {
		t.Fatalf("宽高未记下: %dx%d", got.Width, got.Height)
	}
	if got.Seed == nil || *got.Seed != 7 {
		t.Fatalf("Seed 未记下: %v", got.Seed)
	}
	if got.UpstreamModel != "flux-2-max" {
		t.Fatalf("上游模型名未记下: %q", got.UpstreamModel)
	}
}

func TestStubRespectsContextCancellation(t *testing.T) {
	// stub 的默认延迟是 15 秒；ctx 取消时必须立刻返回，否则测试套件会被拖死。
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Generate(ctx, GenerateRequest{Prompt: "a normal prompt", Width: 1, Height: 1})
	if err == nil {
		t.Fatal("ctx 超时应当返回错误")
	}
	// C2：ctx 取消也是"用户可见的失败"，必须与其他上游失败同一分类，否则调用方按
	// errors.Is 判断时行为会随时机变化。
	if !errors.Is(err, ErrUpstream) {
		t.Fatalf("ctx 超时应当归一成 ErrUpstream: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("包装后原错误仍应可判定: %v", err)
	}
	if !strings.Contains(err.Error(), "context") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应当是 context 错误: %v", err)
	}
}
