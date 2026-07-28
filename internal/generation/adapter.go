package generation

import (
	"context"
	"errors"
	"fmt"
)

// ErrUpstream 上游生成失败（模型拒绝、超时、返回错误状态）。调用方据此退款并把
// generation 标成 failed。与"我们自己写错了"区分开：后者应当是 500。
var ErrUpstream = errors.New("upstream generation failed")

// GenerateRequest 只包含**我们自己的**领域概念。
//
// 各 provider 如何把它翻译成自家请求体、又如何从自家响应里挖出图片 URL，全部
// 关在各自的 adapter 内。**不要**为了"统一"往这里加 provider 专属字段——产品
// 要求兼容各官方 API 的功能，那种通用参数结构在第三家一定漏。
type GenerateRequest struct {
	Prompt string
	Width  int
	Height int
	Seed   *int // nil 表示不指定
}

type GenerateResult struct {
	ImageURL string
	// UpstreamID 上游任务 id，落库便于事后对账。
	UpstreamID string
	// UpstreamCost 上游报告的成本，与我们扣的次数是两回事，落库便于核算毛利。
	UpstreamCost int
}

type Adapter interface {
	// Generate 同步返回结果。实现内部负责兜底查询与错误归一化。
	//
	// ctx **必须**是脱离 HTTP 请求生命周期的 context——客户端断开不应该取消
	// 一次已经付过费的生成。
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

// Registry 按 provider 名字选 adapter。
type Registry map[string]Adapter

func (r Registry) Get(provider string) (Adapter, error) {
	a, ok := r[provider]
	if !ok {
		return nil, fmt.Errorf("没有注册 provider %q 的 adapter", provider)
	}
	return a, nil
}
