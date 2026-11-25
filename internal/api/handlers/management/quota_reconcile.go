package management

import (
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ReconcileQuota accepts only a runtime auth index. Provider observations and
// transport destinations are always obtained by the auth manager itself.
func (h *Handler) ReconcileQuota(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "core auth manager unavailable"})
		return
	}
	body, errRead := io.ReadAll(io.LimitReader(c.Request.Body, 4097))
	root := ojson.ParseBytes(body)
	index := root.Get("auth_index")
	fields := 0
	validFields := true
	root.ForEach(func(key, value ojson.Result) bool {
		fields++
		validFields = validFields && key.String() == "auth_index"
		return true
	})
	if errRead != nil || len(body) > 4096 || !ojson.ValidBytes(body) || !root.IsObject() || !validFields || fields != 1 || index.Type != ojson.String || strings.TrimSpace(index.String()) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body must contain only auth_index"})
		return
	}
	result, errReconcile := h.authManager.ReconcileClaudeQuotaByIndex(c.Request.Context(), index.String())
	if result == nil {
		status := http.StatusBadGateway
		state := "error"
		message := "provider quota usage unavailable"
		if errors.Is(errReconcile, coreauth.ErrClaudeQuotaUnsupported) {
			status = http.StatusBadRequest
			message = errReconcile.Error()
		}
		if errors.Is(errReconcile, coreauth.ErrQuotaObservationStale) {
			status = http.StatusConflict
			state = "stale"
			message = errReconcile.Error()
		}
		if errors.Is(errReconcile, coreauth.ErrQuotaAuthNotFound) {
			status = http.StatusNotFound
			message = errReconcile.Error()
		}
		c.JSON(status, gin.H{"status": state, "error": message, "runtime_applied": false})
		return
	}
	payload := gin.H{"status": "ok", "auth_index": result.AuthIndex, "outcome": result.Outcome,
		"observed_at": result.ObservedAt, "models": result.Models, "remaining_quota_blocks": result.RemainingQuotaBlocks, "persistence": "ok"}
	if errors.Is(errReconcile, coreauth.ErrQuotaPersistence) {
		payload["status"] = "partial"
		payload["error"] = errReconcile.Error()
		payload["runtime_applied"] = true
		payload["persistence"] = "failed"
		c.JSON(http.StatusServiceUnavailable, payload)
		return
	}
	c.JSON(http.StatusOK, payload)
}
