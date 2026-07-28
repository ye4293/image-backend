package generation

import (
	"context"
	"fmt"
	"strings"
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
type StubAdapter struct{}

func NewStubAdapter() *StubAdapter { return &StubAdapter{} }

func (s *StubAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	delay, fail := stubBehavior(req.Prompt)

	// 用 select 而不是 time.Sleep：ctx 取消时要立刻返回，否则默认 15 秒的延迟
	// 会把测试套件拖死，而且也不符合真实 adapter 该有的行为。
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return GenerateResult{}, ctx.Err()
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
