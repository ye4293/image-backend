// Package billing 封装 Stripe 调用。
//
// handler 不直接碰 stripe-go：把 SDK 关在这一层里，换版本或加重试时只改一处。
//
// 注意 stripe-go v86 的调用形状：客户端是 stripe.NewClient(key)（不是给
// 包级变量 stripe.Key 赋值），资源方法是 sc.V1Xxx.Create(ctx, params)
// （不是包级的 xxx.New(params)），且所有参数类型都在根包 stripe 下。
package billing

import (
	"errors"

	stripe "github.com/stripe/stripe-go/v86"
)

// ErrNotConfigured 未配置 Stripe。handler 据此返回明确的"计费未启用"而不是 500。
var ErrNotConfigured = errors.New("billing is not configured")

type Client struct {
	sc         *stripe.Client
	appBaseURL string
}

// New 在 secretKey 为空时**返回 nil**，表示计费未配置。
//
// 这是本包与调用方之间的契约：handler 必须先判 nil（配合 ErrNotConfigured
// 返回明确的"计费未启用"），而不是拿着 nil 去调方法导致 500。保持这个契约，
// 不要改成返回一个"空 Client"——那样错误会推迟到真正请求 Stripe 时才暴露。
func New(secretKey, appBaseURL string) *Client {
	if secretKey == "" {
		return nil
	}
	return &Client{sc: stripe.NewClient(secretKey), appBaseURL: appBaseURL}
}
