package settings

import (
	"log"
	"sync/atomic"

	"image-backend/internal/generation"
	"image-backend/internal/storage"
)

// Snapshot 是某一时刻生效的配置值。
//
// 只读：Reload 会整体换掉一个新的 Snapshot 指针，绝不就地修改已发出去的那个。
type Snapshot struct {
	EZLinkAIBaseURL   string
	FluxAPIKey        string
	R2Endpoint        string
	R2AccessKeyID     string
	R2SecretAccessKey string
	R2Bucket          string
	R2PublicBaseURL   string
	AppBaseURL        string

	// adapters 与 storage 是按上面的值构造好的客户端，随快照一起替换。
	adapters generation.Registry
}

// StorageEnabled 五项齐全才算配置好（与 config.StorageEnabled 同一判断）。
func (s *Snapshot) StorageEnabled() bool {
	return s.R2Endpoint != "" && s.R2AccessKeyID != "" && s.R2SecretAccessKey != "" &&
		s.R2Bucket != "" && s.R2PublicBaseURL != ""
}

// Runtime 持有当前生效的配置与客户端，支持不重启热替换。
//
// 用 atomic.Pointer 而不是读写锁：读方是每一个请求（生成请求会持有数十秒），
// 加锁读会让写方在改配置时被长请求挡住，而无锁读没有这个问题。写极少发生，
// 每次重建全部客户端的成本可以忽略。
type Runtime struct {
	store *Store
	snap  atomic.Pointer[Snapshot]
}

func NewRuntime(store *Store) (*Runtime, error) {
	rt := &Runtime{store: store}
	if err := rt.Reload(); err != nil {
		return nil, err
	}
	return rt, nil
}

// Reload 从库里重读配置、重建客户端，然后原子替换快照。
func (rt *Runtime) Reload() error {
	vals, err := rt.store.All()
	if err != nil {
		return err
	}
	s := &Snapshot{
		EZLinkAIBaseURL:   valOr(vals, "ezlinkaiBaseUrl", "https://api.ezlinkai.com"),
		FluxAPIKey:        vals["fluxApiKey"],
		R2Endpoint:        vals["r2Endpoint"],
		R2AccessKeyID:     vals["r2AccessKeyId"],
		R2SecretAccessKey: vals["r2SecretAccessKey"],
		R2Bucket:          vals["r2Bucket"],
		R2PublicBaseURL:   vals["r2PublicBaseUrl"],
		AppBaseURL:        vals["appBaseUrl"],
	}
	s.adapters = buildAdapters(s)
	rt.snap.Store(s)
	return nil
}

func (rt *Runtime) Snapshot() *Snapshot           { return rt.snap.Load() }
func (rt *Runtime) Adapters() generation.Registry { return rt.snap.Load().adapters }
func (rt *Runtime) StorageEnabled() bool          { return rt.snap.Load().StorageEnabled() }
func (rt *Runtime) AppBaseURL() string            { return rt.snap.Load().AppBaseURL }

// buildAdapters 与原先 server.BuildAdapters 同构，只是配置来自快照而非 env。
//
// 每个 adapter 都被 StoringAdapter 包一层：新增 provider 自动获得转存，不依赖
// 谁记得加代码。
func buildAdapters(s *Snapshot) generation.Registry {
	return generation.Registry{
		"flux": generation.NewStoringAdapter(buildFlux(s), buildStorage(s)),
	}
}

func buildFlux(s *Snapshot) generation.Adapter {
	if s.FluxAPIKey == "" {
		// 退化成 stub 而不是拿空 key 去打上游：后者会让每次生成都以"上游认证
		// 失败"收场，而次数已经扣了。
		log.Println("settings: 未配置 fluxApiKey，使用 stub adapter（返回占位图）")
		return generation.NewStubAdapter()
	}
	return generation.NewFluxAdapter(s.EZLinkAIBaseURL, s.FluxAPIKey)
}

func buildStorage(s *Snapshot) storage.Storage {
	if !s.StorageEnabled() {
		log.Println("settings: R2 未完整配置，图片不转存——image_url 存的是上游临时链接，约一小时后失效")
		return storage.NoopStorage{}
	}
	return storage.NewR2Storage(
		s.R2Endpoint, s.R2AccessKeyID, s.R2SecretAccessKey,
		s.R2Bucket, s.R2PublicBaseURL,
	)
}

func valOr(m map[string]string, key, def string) string {
	if v := m[key]; v != "" {
		return v
	}
	return def
}

// Validate 报告当前生效配置里的问题，供启动时打告警用。
//
// **返回问题列表而不是 error，也不让调用方 Fatal。** 库里的值可能是上一个管理员
// 改坏的，此时拒绝启动等于让一次误操作把服务打死，而正确的行为是带着告警起来、
// 让管理员能登录进去改回来（见设计文档 §2.5）。
func (rt *Runtime) Validate() []string {
	s := rt.Snapshot()
	var problems []string
	for _, kv := range []struct{ k, v string }{
		{"r2PublicBaseUrl", s.R2PublicBaseURL},
		{"appBaseUrl", s.AppBaseURL},
		{"r2Endpoint", s.R2Endpoint},
		{"ezlinkaiBaseUrl", s.EZLinkAIBaseURL},
	} {
		if err := Validate(kv.k, kv.v); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}
