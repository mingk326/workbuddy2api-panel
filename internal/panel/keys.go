// keys.go 多密钥管理接口：列表（脱敏）/ 新增 / 修改 / 删除。
//
// 与配置页的分工：配置页仍是「整份 config 表单」；本文件只负责 api_keys 数组的
// 读-改-写，写完经注入的 SaveConfig 闭包走同一套校验/落盘/热生效链路。
// 旧版 config.api_key 以 legacy 身份只读展示（恒为管理员，不参与增删改），
// 想统一管理只需在配置页清空它并新建密钥。
package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// panelKey 与 cmd/server.APIKeyEntry 的 JSON 形状一一对应。
// 面板不直接依赖 main 包类型，避免反向依赖。
type panelKey struct {
	ID            string            `json:"id"`
	Key           string            `json:"key"`
	Name          string            `json:"name,omitempty"`
	Admin         bool              `json:"admin,omitempty"`
	DefaultEffort string            `json:"thinking_default_effort,omitempty"`
	PerModel      map[string]string `json:"thinking_per_model,omitempty"`
}

// keyView 列表展示项（密钥脱敏，绝不回传完整值）。
type keyView struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Masked        string            `json:"masked"`
	Admin         bool              `json:"admin"`
	Legacy        bool              `json:"legacy,omitempty"`
	DefaultEffort string            `json:"thinking_default_effort"`
	PerModel      map[string]string `json:"thinking_per_model,omitempty"`
}

type keyUpsertReq struct {
	ID            string            `json:"id"`
	Key           string            `json:"key"` // 新增时可选：空 = 自动生成
	Name          string            `json:"name"`
	Admin         bool              `json:"admin"`
	DefaultEffort string            `json:"thinking_default_effort"`
	PerModel      map[string]string `json:"thinking_per_model"`
}

type keyUpdateReq struct {
	ID            string            `json:"id"`
	Name          *string           `json:"name"`
	Admin         *bool             `json:"admin"`
	DefaultEffort *string           `json:"thinking_default_effort"`
	PerModel      map[string]string `json:"thinking_per_model"` // nil = 不改；{} = 清空
}

type keyDeleteReq struct {
	ID string `json:"id"`
}

// keysList 返回密钥清单（脱敏）+ 是否启用鉴权。
func (p *Panel) keysList(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	m, err := p.readConfigMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys := configKeyList(m)
	legacy := stringField(m, "api_key")
	views := make([]keyView, 0, len(keys)+1)
	for _, k := range keys {
		views = append(views, keyView{
			ID: k.ID, Name: k.Name, Masked: maskKey(k.Key), Admin: k.Admin,
			DefaultEffort: k.DefaultEffort, PerModel: k.PerModel,
		})
	}
	if legacy != "" {
		views = append(views, keyView{
			ID: "legacy", Name: "旧版 api_key（config.api_key）", Masked: maskKey(legacy),
			Admin: true, Legacy: true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"auth_required": p.authRequired(),
		"keys":          views,
	})
}

// keysCreate 新建密钥：密钥留空则生成 sk- 前缀随机串；完整值只在本次响应返回。
func (p *Panel) keysCreate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var req keyUpsertReq
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	secret := strings.TrimSpace(req.Key)
	if secret == "" {
		var err error
		if secret, err = generateAPIKey(); err != nil {
			writeErr(w, http.StatusInternalServerError, "生成密钥失败: "+err.Error())
			return
		}
	}
	if len(secret) < 8 {
		writeErr(w, http.StatusBadRequest, "密钥长度至少 8 个字符")
		return
	}

	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	m, err := p.readConfigMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys := configKeyList(m)
	if legacy := stringField(m, "api_key"); legacy != "" && legacy == secret {
		writeErr(w, http.StatusBadRequest, "该密钥与 config.api_key 重复")
		return
	}
	for _, k := range keys {
		if k.Key == secret {
			writeErr(w, http.StatusBadRequest, "该密钥已存在（id="+k.ID+"）")
			return
		}
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		id = "key-" + shortHash(secret)
	}
	if id == "legacy" {
		writeErr(w, http.StatusBadRequest, "id \"legacy\" 为旧版 api_key 保留标识，请换一个")
		return
	}
	for _, k := range keys {
		if k.ID == id {
			writeErr(w, http.StatusBadRequest, "id 已存在："+id)
			return
		}
	}
	entry := panelKey{
		ID: id, Key: secret, Name: strings.TrimSpace(req.Name), Admin: req.Admin,
		DefaultEffort: strings.ToLower(strings.TrimSpace(req.DefaultEffort)),
		PerModel:      normalizeEffortMap(req.PerModel),
	}
	keys = append(keys, entry)
	if err := p.saveKeys(m, keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"id":    id,
		"key":   secret, // 唯一一次回显完整密钥
		"entry": keyView{ID: id, Name: entry.Name, Masked: maskKey(secret), Admin: entry.Admin, DefaultEffort: entry.DefaultEffort, PerModel: entry.PerModel},
	})
}

// keysUpdate 修改备注/管理员标记/思考档位（不支持改密钥本体）。
func (p *Panel) keysUpdate(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var req keyUpdateReq
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" || id == "legacy" {
		writeErr(w, http.StatusBadRequest, "旧版 api_key 不可在此修改（请在配置页调整或清空后重建）")
		return
	}

	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	m, err := p.readConfigMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys := configKeyList(m)
	idx := -1
	for i := range keys {
		if keys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "密钥不存在："+id)
		return
	}
	if req.Name != nil {
		keys[idx].Name = strings.TrimSpace(*req.Name)
	}
	if req.Admin != nil {
		keys[idx].Admin = *req.Admin
	}
	if req.DefaultEffort != nil {
		keys[idx].DefaultEffort = strings.ToLower(strings.TrimSpace(*req.DefaultEffort))
	}
	if req.PerModel != nil {
		keys[idx].PerModel = normalizeEffortMap(req.PerModel)
	}
	if !hasAdmin(stringField(m, "api_key"), keys) {
		writeErr(w, http.StatusBadRequest, "至少保留一把管理员密钥（admin=true），否则面板将无法登录")
		return
	}
	if err := p.saveKeys(m, keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keysDelete 删除密钥：禁止删 legacy、禁止删到一把不剩、禁止删掉最后一把 admin。
func (p *Panel) keysDelete(w http.ResponseWriter, r *http.Request) {
	if p.cfg.ConfigPath == "" || p.cfg.SaveConfig == nil {
		writeErr(w, http.StatusNotImplemented, "config api not available")
		return
	}
	var req keyDeleteReq
	if err := readJSONBody(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" || id == "legacy" {
		writeErr(w, http.StatusBadRequest, "旧版 api_key 不可在此删除（请在配置页清空 api_key）")
		return
	}

	p.keyMu.Lock()
	defer p.keyMu.Unlock()
	m, err := p.readConfigMap()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	keys := configKeyList(m)
	out := make([]panelKey, 0, len(keys))
	found := false
	for _, k := range keys {
		if k.ID == id {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "密钥不存在："+id)
		return
	}
	legacy := stringField(m, "api_key")
	if legacy == "" && len(out) == 0 {
		writeErr(w, http.StatusBadRequest, "至少保留一把密钥，否则网关会变成无鉴权服务")
		return
	}
	if !hasAdmin(legacy, out) {
		writeErr(w, http.StatusBadRequest, "至少保留一把管理员密钥（admin=true），否则面板将无法登录")
		return
	}
	if err := p.saveKeys(m, out); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// readConfigMap 读取当前 config.json 为通用 map（保留未知键）。
func (p *Panel) readConfigMap() (map[string]any, error) {
	raw, err := os.ReadFile(p.cfg.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	return m, nil
}

// saveKeys 写回 api_keys 并走 SaveConfig（校验/落盘/热生效）。
func (p *Panel) saveKeys(m map[string]any, keys []panelKey) error {
	m["api_keys"] = keys
	raw, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	_, err = p.cfg.SaveConfig(raw)
	return err
}

// configKeyList 从配置 map 解析 api_keys 数组。
func configKeyList(m map[string]any) []panelKey {
	raw, ok := m["api_keys"]
	if !ok || raw == nil {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var keys []panelKey
	if json.Unmarshal(b, &keys) != nil {
		return nil
	}
	return keys
}

func stringField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// hasAdmin 是否存在管理员：legacy api_key 恒为管理员。
func hasAdmin(legacy string, keys []panelKey) bool {
	if legacy != "" {
		return true
	}
	for _, k := range keys {
		if k.Admin {
			return true
		}
	}
	return false
}

func normalizeEffortMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		model := strings.TrimSpace(k)
		effort := strings.ToLower(strings.TrimSpace(v))
		if model == "" || effort == "" {
			continue
		}
		out[model] = effort
	}
	return out
}

// generateAPIKey 生成 sk- 前缀的随机密钥（24 字节熵，base64url 无填充）。
func generateAPIKey() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(b), nil
}

func shortHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:8]
}

// maskKey 脱敏：仅保留前 6 位与后 4 位；短密钥整段隐藏。
func maskKey(s string) string {
	if len(s) <= 12 {
		return "****"
	}
	return s[:6] + "…" + s[len(s)-4:]
}

// readJSONBody 读取并解析 JSON 请求体（上限 64 KiB）。
func readJSONBody(r *http.Request, v any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("parse body: %w", err)
	}
	return nil
}
