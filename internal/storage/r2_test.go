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

func TestNoopStorageAlwaysReturnsNotConfigured(t *testing.T) {
	// Noop **返回错误而不是返回原 URL**：这样"未配置"与"配置了但失败"在装饰器里
	// 走同一条代码路径。否则降级分支只在生产才会被走到，而那是最不该第一次运行
	// 的地方。
	_, err := NoopStorage{}.Put(context.Background(), "g/a.png", "image/png", []byte("x"))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("want ErrNotConfigured, got %v", err)
	}
}
