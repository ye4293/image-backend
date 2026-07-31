// Package storage 把生成好的图片转存到我们自己控制的对象存储。
//
// 存在理由：上游返回的图片 URL 指向它自己的 CDN，约一小时后失效。不转存的话
// 历史记录里全是死链，而用户为那些图付过费。
package storage

import (
	"context"
	"errors"
)

// ErrNotConfigured NoopStorage 的固定返回，调用方据此走降级路径。
var ErrNotConfigured = errors.New("storage is not configured")

type Storage interface {
	// Put 上传并返回可公开访问的永久 URL。
	//
	// body 收 []byte 而不是 io.Reader：aws-sdk-go-v2 的 PutObject 需要可重放的
	// body 才能签名和重试，给它一个不可 seek 的 reader 会让 SDK 自己先缓冲一遍
	// ——同一份数据在内存里两份。反正上游就是一张图、调用方本来就要限大小，
	// 直接收字节更诚实。
	Put(ctx context.Context, key, contentType string, body []byte) (string, error)
}
