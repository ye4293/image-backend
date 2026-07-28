package generation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FluxAdapter 对接 ezlinkai 的 Flux 端点。
//
// 上游有两处反直觉的地方，都是 2026-07-28 实测所得，不要"顺手统一"掉：
//
//  1. **提交响应里的 `polling_url` 装的是最终图片 URL**，不是给你去轮询的地址。
//     ezlinkai 在内部替我们轮询了 BFL，挂住连接直到出图（实测约 21 秒）。
//  2. **两个端点的认证头不一样**：提交用 `x-key`，`get_result` 用
//     `Authorization: Bearer`。改成统一会 401。
type FluxAdapter struct {
	baseURL       string
	apiKey        string
	upstreamModel string
	client        *http.Client
}

func NewFluxAdapter(baseURL, apiKey, upstreamModel string) *FluxAdapter {
	return &FluxAdapter{
		baseURL:       strings.TrimRight(baseURL, "/"),
		apiKey:        apiKey,
		upstreamModel: upstreamModel,
		// 不设 Timeout：超时由调用方通过 ctx 控制，那样才能和"脱离请求的
		// context"配合。这里再设一个会变成两个互相打架的期限。
		client: &http.Client{},
	}
}

type fluxSubmitResponse struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	PollingURL string `json:"polling_url"`
	Cost       int    `json:"cost"`
}

type fluxResultResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result struct {
		Sample string `json:"sample"`
	} `json:"result"`
}

const fluxStatusReady = "Ready"

func (a *FluxAdapter) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	body := map[string]any{
		"prompt":           req.Prompt,
		"width":            req.Width,
		"height":           req.Height,
		"output_format":    "jpeg",
		"safety_tolerance": 2,
	}
	// Seed 为 nil 时**不能**塞 0 进去——0 是一个合法的 seed，会让"不指定"变成
	// "每次都用同一个 seed"，用户会发现同样的 prompt 永远出同一张图。
	if req.Seed != nil {
		body["seed"] = *req.Seed
	}

	sub, err := a.submit(ctx, body)
	if err != nil {
		return GenerateResult{}, err
	}

	if sub.Status == fluxStatusReady && sub.PollingURL != "" {
		return GenerateResult{
			ImageURL:     sub.PollingURL,
			UpstreamID:   sub.ID,
			UpstreamCost: sub.Cost,
		}, nil
	}

	// 未就绪：走兜底查询。这条路径在实测中没出现过（提交总是直接返回 Ready），
	// 但一旦 ezlinkai 内部超时先返回，没有它就是扣了次数拿不到图且无从补救。
	url, err := a.getResult(ctx, sub.ID)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ImageURL: url, UpstreamID: sub.ID, UpstreamCost: sub.Cost}, nil
}

func (a *FluxAdapter) submit(ctx context.Context, body map[string]any) (fluxSubmitResponse, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	endpoint := fmt.Sprintf("%s/flux/v1/%s", a.baseURL, a.upstreamModel)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fluxSubmitResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("x-key", a.apiKey) // 提交端点用 x-key（见类型注释）

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交请求失败: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 读取响应失败: %v", ErrUpstream, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 提交返回 %d: %s",
			ErrUpstream, resp.StatusCode, truncate(string(payload), 300))
	}
	var out fluxSubmitResponse
	if err := json.Unmarshal(payload, &out); err != nil {
		return fluxSubmitResponse{}, fmt.Errorf("%w: 响应不是合法 JSON: %s",
			ErrUpstream, truncate(string(payload), 300))
	}
	return out, nil
}

// getResult 轮询兜底端点直到就绪或 ctx 到期。
func (a *FluxAdapter) getResult(ctx context.Context, id string) (string, error) {
	endpoint := fmt.Sprintf("%s/flux/v1/get_result?id=%s", a.baseURL, id)
	for {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return "", err
		}
		// 兜底端点用 Bearer，与提交端点相反（见类型注释）。
		httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

		resp, err := a.client.Do(httpReq)
		if err != nil {
			return "", fmt.Errorf("%w: 兜底查询失败: %v", ErrUpstream, err)
		}
		payload, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return "", fmt.Errorf("%w: 读取兜底响应失败: %v", ErrUpstream, readErr)
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return "", fmt.Errorf("%w: 兜底查询返回 %d: %s",
				ErrUpstream, resp.StatusCode, truncate(string(payload), 300))
		}
		var out fluxResultResponse
		if err := json.Unmarshal(payload, &out); err != nil {
			return "", fmt.Errorf("%w: 兜底响应不是合法 JSON: %s",
				ErrUpstream, truncate(string(payload), 300))
		}
		if out.Status == fluxStatusReady && out.Result.Sample != "" {
			return out.Result.Sample, nil
		}

		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
