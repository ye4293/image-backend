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

func TestStubRespectsContextCancellation(t *testing.T) {
	// stub 的默认延迟是 15 秒；ctx 取消时必须立刻返回，否则测试套件会被拖死。
	s := NewStubAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := s.Generate(ctx, GenerateRequest{Prompt: "a normal prompt", Width: 1, Height: 1})
	if err == nil {
		t.Fatal("ctx 超时应当返回错误")
	}
	if !strings.Contains(err.Error(), "context") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("应当是 context 错误: %v", err)
	}
}
