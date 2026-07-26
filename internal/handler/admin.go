package handler

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/user/go2api/internal/config"
	"github.com/user/go2api/internal/keypool"
	"github.com/user/go2api/internal/store"
)

// Admin holds dependencies for the /admin/* endpoints.
type Admin struct {
	Pool  *keypool.Pool
	Store *store.DB
}

// Keys handles GET /admin/keys and returns the snapshot of all configured keys.
func (a *Admin) Keys(w http.ResponseWriter, r *http.Request) {
	views, err := a.Pool.Snapshot(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "获取 Key 列表失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": views})
}

// AddKey handles POST /admin/keys and inserts a new key.
func (a *Admin) AddKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		Label    string `json:"label"`
		APIKey   string `json:"api_key"`
		Weight   int    `json:"weight"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON 格式错误")
		return
	}
	if body.Label == "" || body.APIKey == "" {
		writeJSONError(w, http.StatusBadRequest, "名称和 Go Key 不能为空")
		return
	}
	if err := a.Pool.AddKey(r.Context(), config.KeyConfig{
		ID:       body.ID,
		Label:    body.Label,
		APIKey:   body.APIKey,
		Weight:   body.Weight,
		Disabled: body.Disabled,
	}); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "添加 Go Key 失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "ok"})
}

// ToggleKey handles PATCH /admin/keys/:id and updates the disabled flag.
func (a *Admin) ToggleKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	if id == "" || strings.Contains(id, "/") {
		writeJSONError(w, http.StatusBadRequest, "缺少 ID")
		return
	}
	var body struct {
		Disabled *bool `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "JSON 格式错误")
		return
	}
	if body.Disabled == nil {
		writeJSONError(w, http.StatusBadRequest, "缺少 disabled 字段")
		return
	}
	if err := a.Pool.SetDisabled(r.Context(), id, *body.Disabled); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "更新失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DeleteKey handles DELETE /admin/keys/:id.
func (a *Admin) DeleteKey(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	if id == "" || strings.Contains(id, "/") {
		writeJSONError(w, http.StatusBadRequest, "缺少 ID")
		return
	}
	if err := a.Pool.RemoveKey(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "删除失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Stats handles GET /admin/stats and returns counters for the past 24h.
func (a *Admin) Stats(w http.ResponseWriter, r *http.Request) {
	st, err := a.Store.StatsSummary(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "获取统计失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// FlushCache handles POST /admin/cache/flush.
func (a *Admin) FlushCache(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.CacheFlush(r.Context()); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "清空失败:"+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
