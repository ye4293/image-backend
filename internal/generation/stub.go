package generation

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// StubImageURL 是 stub 返回的占位图。前端 public/ 下已有同名文件。
const StubImageURL = "/placeholder-generation.svg"

// StubAdapter 在没有配置上游 key 时顶替真实 adapter。
//
// 存在理由：接真上游后，端到端测试每跑一次都会真调 Flux——每次约 21 秒、每次
// 花钱。stub 让 CI 与本地测试既快又免费，同时保留 M2 前端假数据那套**确定性**
// 关键词触发，这样端到端测试仍能稳定复现失败路径。
//
// 关键词优先级：fail > slow > quick > 默认。与前端 M2 的行为保持一致。
type StubAdapter struct {
	mu      sync.Mutex
	lastReq *GenerateRequest
}

func NewStubAdapter() *StubAdapter { return &StubAdapter{} }

// LastRequest 返回最后一次收到的请求。
//
// stub 记下入参是为了让**接口层**测试能发现 handler 把画幅译错、漏传上游模型名或漏传
// Seed。一个忽略入参的 stub 会让这类 bug 完全隐形——上层测试只能看到"成功了"。
func (s *StubAdapter) LastRequest() (GenerateRequest, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastReq == nil {
		return GenerateRequest{}, false
	}
	return *s.lastReq, true
}

func (s *StubAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	s.mu.Lock()
	captured := req
	s.lastReq = &captured
	s.mu.Unlock()

	delay, fail := stubBehavior(req.Prompt)

	// 用 select 而不是 time.Sleep：ctx 取消时要立刻返回，否则默认 15 秒的延迟
	// 会把测试套件拖死，而且也不符合真实 adapter 该有的行为。
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		// 双 %w：ctx 取消同样是"用户可见的失败"，分类必须与其他上游失败一致（见
		// ErrUpstream 的注释），同时保留原错误能被 errors.Is 判定。
		return GenerateResult{}, fmt.Errorf("%w: 等待 stub 结果被取消: %w", ErrUpstream, ctx.Err())
	}

	if fail {
		return GenerateResult{}, fmt.Errorf("%w: stub 模拟的上游失败", ErrUpstream)
	}
	return GenerateResult{
		ImageURL:     StubImageURL,
		UpstreamID:   "stub-" + fmt.Sprint(time.Now().UnixNano()),
		UpstreamCost: 0,
	}, nil
}

func stubBehavior(prompt string) (delay time.Duration, fail bool) {
	p := strings.ToLower(prompt)
	switch {
	case strings.Contains(p, "fail"):
		return 800 * time.Millisecond, true
	case strings.Contains(p, "slow"):
		return 90 * time.Second, false
	case strings.Contains(p, "quick"):
		return 200 * time.Millisecond, false
	default:
		return 15 * time.Second, false
	}
}
