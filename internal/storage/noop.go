package storage

import "context"

// NoopStorage 在没有配置 R2 时顶替 R2Storage。
//
// **它返回错误而不是返回传入的原 URL。** 这样"未配置"与"配置了但上传失败"在
// 调用方那里是同一条代码路径——降级分支于是在本地开发天天被走到，而不是只在
// 生产才第一次运行。
type NoopStorage struct{}

func (NoopStorage) Put(context.Context, string, string, []byte) (string, error) {
	return "", ErrNotConfigured
}
