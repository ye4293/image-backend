package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"image-backend/internal/settings"
)

// AdminSettingsHandler 让运营在后台查看和修改动态配置。
//
// GET  /api/v1/admin/settings  — 返回所有配置项；secret 只露 configured + masked，
//
//	绝不回传明文（§2.2）。
//
// PATCH /api/v1/admin/settings — 局部更新；任何校验失败整体 400 不 Reload（§2.5）。
type AdminSettingsHandler struct {
	Store   *settings.Store
	Runtime *settings.Runtime
}

// buildSettingsResponse 把 Store.All() 的明文映射转成 API 响应形状。
//
// secret 项只输出 configured + masked：一旦回传明文，设置页就是一个凭据泄露端点。
// non-secret 项输出 value。
func buildSettingsResponse(vals map[string]string, rt *settings.Runtime) gin.H {
	result := make(map[string]any, len(settings.Specs))
	for _, spec := range settings.Specs {
		v := vals[spec.Key]
		if spec.Secret {
			configured := v != ""
			result[spec.Key] = gin.H{
				"configured": configured,
				"masked":     settings.Mask(v),
			}
		} else {
			result[spec.Key] = gin.H{"value": v}
		}
	}
	return gin.H{
		"settings":       result,
		"storageEnabled": rt.StorageEnabled(),
	}
}

func (h *AdminSettingsHandler) Get(c *gin.Context) {
	vals, err := h.Store.All()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, buildSettingsResponse(vals, h.Runtime))
}

// Patch 局部更新配置。
//
// 两阶段：先对所有 key 校验（未知 key、格式错误），全通过后再逐项写入，最后 Reload。
// 这样避免"改了一半"：第 3 项校验失败时前两项已落库但 Reload 还没发生，会造成
// 内存与数据库短暂不一致。预校验在数据接触数据库之前就返回 400，数据库保持原状。
func (h *AdminSettingsHandler) Patch(c *gin.Context) {
	var body map[string]string
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "invalid request body"})
		return
	}
	if len(body) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "request body contains no settings"})
		return
	}

	// Phase 1: validate everything before touching the DB.
	for key, value := range body {
		if _, ok := settings.Lookup(key); !ok {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": "unknown setting key: " + key})
			return
		}
		if err := settings.Validate(key, value); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"code": errCodeBadRequest, "message": err.Error()})
			return
		}
	}

	// Phase 2: write (validation already passed, so Set errors are unexpected).
	for key, value := range body {
		if err := h.Store.Set(key, value); err != nil {
			// Set re-validates internally; if we land here it is an internal error
			// (e.g. encryption failure), not a bad request.
			c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": err.Error()})
			return
		}
	}

	// Reload only after all writes succeed.
	if err := h.Runtime.Reload(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "reload failed: " + err.Error()})
		return
	}

	vals, err := h.Store.All()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"code": errCodeInternal, "message": "internal error"})
		return
	}
	c.JSON(http.StatusOK, buildSettingsResponse(vals, h.Runtime))
}
