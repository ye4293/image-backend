// 这些测试打的是 httptest.Server，它照单全收任何 Authorization 头、只回裸状态码。
// 因此它们验证的只有两件事：发出去的请求形状（方法、路径、Content-Type、字节原样
// 不被改写）和非 2xx 时错误往上传。
//
// **以下都没有被覆盖，必须拿真 R2 凭证人工验证：**
//   - 请求签名——签名算错的话这些测试照样全绿；
//   - R2 的 XML 错误体解析（<Error><Code>AccessDenied</Code>...）；
//   - ETag 处理。
//
// 也就是说"storage 的测试过了"不等于"转存这条链路是通的"。
package storage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestR2StoragePutSendsObjectAndReturnsPublicURL(t *testing.T) {
	var gotMethod, gotPath, gotType string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com")
	url, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("PNGDATA"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	// 返回的 URL **必须**由公开域拼出，不能是 S3 endpoint——后者不允许匿名读，
	// 用它拼出来的每一个链接都会 401。这正是 config.ValidateStorage 要防的错。
	if url != "https://img.example.com/g/abc.png" {
		t.Errorf("返回 URL: got %q", url)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("方法: got %q, want PUT", gotMethod)
	}
	// UsePathStyle 之下路径是 /<bucket>/<key>。
	if gotPath != "/images/g/abc.png" {
		t.Errorf("路径: got %q, want /images/g/abc.png", gotPath)
	}
	if gotType != "image/png" {
		t.Errorf("Content-Type: got %q", gotType)
	}
	if string(gotBody) != "PNGDATA" {
		t.Errorf("body: got %q", gotBody)
	}
}

func TestR2StoragePutPropagatesUpstreamFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com")
	if _, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("x")); err == nil {
		t.Fatal("上传失败必须返回错误——吞掉它会让库里存下一个指向不存在对象的永久 URL")
	}
}

func TestR2StorageTrimsTrailingSlashOnPublicBase(t *testing.T) {
	// 运维在 R2_PUBLIC_BASE_URL 末尾多打一个斜杠是必然会发生的事，
	// 而后果是每个 URL 里出现 // ——有些 CDN 会 404。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com/")
	url, err := s.Put(context.Background(), "g/abc.png", "image/png", []byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if strings.Contains(strings.TrimPrefix(url, "https://"), "//") {
		t.Errorf("URL 里出现重复斜杠: %q", url)
	}
}

func TestR2StorageTrimsLeadingSlashOnKey(t *testing.T) {
	// 调用方用字符串拼 key，多一个前导斜杠同样是必然会发生的事。它会同时污染
	// 对象键和存进库里的 URL，所以两边都断言：归一化必须发生在进 SDK 之前，
	// 否则对象存在 //g/abc.png 而 URL 指向 /g/abc.png，两者永远对不上。
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := NewR2Storage(srv.URL, "ak", "sk", "images", "https://img.example.com")
	url, err := s.Put(context.Background(), "/g/abc.png", "image/png", []byte("x"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if url != "https://img.example.com/g/abc.png" {
		t.Errorf("返回 URL: got %q, want https://img.example.com/g/abc.png", url)
	}
	if gotPath != "/images/g/abc.png" {
		t.Errorf("对象键路径: got %q, want /images/g/abc.png", gotPath)
	}
}

func TestNoopStorageAlwaysReturnsNotConfigured(t *testing.T) {
	// Noop **返回错误而不是返回原 URL**：这样"未配置"与"配置了但失败"在装饰器里
	// 走同一条代码路径。否则降级分支只在生产才会被走到，而那是最不该第一次运行
	// 的地方。
	_, err := NoopStorage{}.Put(context.Background(), "g/a.png", "image/png", []byte("x"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
