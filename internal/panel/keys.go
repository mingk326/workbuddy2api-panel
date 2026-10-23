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
	// AllowedModels 可用模型白名单：空 = 不限制；裸名对所有域生效，
	// 带 cn:/global: 前缀只限定该域（与 httpauth.Key.AllowsModel 同口径）。
	AllowedModels []string `json:"allowed_models,omitempty"`
	// DailyTokenLimit / DailyRequestLimit 每日配额（CST 自然日）；0 = 不限制。
	DailyTokenLimit   int64 `json:"daily_token_limit,omitempty"`
	DailyRequestLimit int64 `json:"daily_request_limit,omitempty"`
	// RPMLimit / MaxConcurrency 即时限流：滑动 60 秒请求数与在途并发；0 = 不限制。
	RPMLimit       int `json:"rpm_limit,omitempty"`
	MaxConcurrency int `json:"max_concurrency,omitempty"`
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
	AllowedModels []string          `json:"allowed_models"`
	// 每日配额与今日用量（CST 自然日；用量来自 keyquota，0 = 尚无记录）。
	DailyTokenLimit   int64 `json:"daily_token_limit"`
	DailyRequestLimit int64 `json:"daily_request_limit"`
	TodayTokens       int64 `json:"today_tokens"`
	TodayRequests     int64 `json:"today_requests"`
	// 即时限流与当前用量：仅当配置了对应上限时统计（0 = 未配置）。
	RPMLimit        int `json:"rpm_limit"`
	MaxConcurrency  int `json:"max_concurrency"`
	RPMUsed         int `json:"rpm_used"`
	ConcurrencyUsed int `json:"concurrency_used"`
}

type keyUpsertReq struct {
	ID                string            `json:"id"`
	Key               string            `json:"key"` // 新增时可选：空 = 自动生成
	Name              string            `json:"name"`
	Admin             bool              `json:"admin"`
	DefaultEffort     string            `json:"thinking_default_effort"`
	PerModel          map[string]string `json:"thinking_per_model"`
	AllowedModels     []string          `json:"allowed_models"`
	DailyTokenLimit   int64             `json:"daily_token_limit"`
	DailyRequestLimit int64             `json:"daily_request_limit"`
	RPMLimit          int               `json:"rpm_limit"`
	MaxConcurrency    int               `json:"max_concurrency"`
}

type keyUpdateReq struct {
	ID                string            `json:"id"`
	Name              *string           `json:"name"`
	Admin             *bool             `json:"admin"`
	DefaultEffort     *string           `json:"thinking_default_effort"`
	PerModel          map[string]string `json:"thinking_per_model"`  // nil = 不改；{} = 清空
	AllowedModels     []string          `json:"allowed_models"`      // nil = 不改；[] = 清空（= 不限制）
	DailyTokenLimit   *int64            `json:"daily_token_limit"`   // nil = 不改；0 = 不限制
	DailyRequestLimit *int64            `json:"daily_request_limit"` // nil = 不改；0 = 不限制
	RPMLimit          *int              `json:"rpm_limit"`           // nil = 不改；0 = 不限制
	MaxConcurrency    *int              `json:"max_concurrency"`     // nil = 不改；0 = 不限制
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
		tok, req := p.keyUsage(k.ID)
		rpmUsed, inflight := p.keyLive(k.ID)
		views = append(views, keyView{
			ID: k.ID, Name: k.Name, Masked: maskKey(k.Key), Admin: k.Admin,
			DefaultEffort: k.DefaultEffort, PerModel: k.PerModel, AllowedModels: k.AllowedModels,
			DailyTokenLimit: k.DailyTokenLimit, DailyRequestLimit: k.DailyRequestLimit,
			RPMLimit: k.RPMLimit, MaxConcurrency: k.MaxConcurrency,
			RPMUsed: rpmUsed, ConcurrencyUsed: inflight,
			TodayTokens: tok, TodayRequests: req,
		})
	}
	if legacy != "" {
		tok, req := p.keyUsage("legacy")
		views = append(views, keyView{
			ID: "legacy", Name: "旧版 api_key（config.api_key）", Masked: maskKey(legacy),
			Admin: true, Legacy: true, TodayTokens: tok, TodayRequests: req,
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
	if req.DailyTokenLimit < 0 || req.DailyRequestLimit < 0 {
		writeErr(w, http.StatusBadRequest, "每日配额不能为负（0 = 不限制）")
		return
	}
	if req.RPMLimit < 0 || req.MaxConcurrency < 0 {
		writeErr(w, http.StatusBadRequest, "RPM / 并发上限不能为负（0 = 不限制）")
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
		DefaultEffort:     strings.ToLower(strings.TrimSpace(req.DefaultEffort)),
		PerModel:          normalizeEffortMap(req.PerModel),
		AllowedModels:     normalizeModelList(req.AllowedModels),
		DailyTokenLimit:   req.DailyTokenLimit,
		DailyRequestLimit: req.DailyRequestLimit,
		RPMLimit:          req.RPMLimit,
		MaxConcurrency:    req.MaxConcurrency,
	}
	keys = append(keys, entry)
	if err := p.saveKeys(m, keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"id":  id,
		"key": secret, // 唯一一次回显完整密钥
		"entry": keyView{ID: id, Name: entry.Name, Masked: maskKey(secret), Admin: entry.Admin,
			DefaultEffort: entry.DefaultEffort, PerModel: entry.PerModel, AllowedModels: entry.AllowedModels,
			DailyTokenLimit: entry.DailyTokenLimit, DailyRequestLimit: entry.DailyRequestLimit,
			RPMLimit: entry.RPMLimit, MaxConcurrency: entry.MaxConcurrency},
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
	// nil = 不改；[]（显式空数组）= 清空白名单 = 不限制。
	if req.AllowedModels != nil {
		keys[idx].AllowedModels = normalizeModelList(req.AllowedModels)
	}
	// nil = 不改；0 = 恢复不限制。负数拒绝（与启动校验同口径）。
	if req.DailyTokenLimit != nil {
		if *req.DailyTokenLimit < 0 {
			writeErr(w, http.StatusBadRequest, "每日 Token 上限不能为负（0 = 不限制）")
			return
		}
		keys[idx].DailyTokenLimit = *req.DailyTokenLimit
	}
	if req.DailyRequestLimit != nil {
		if *req.DailyRequestLimit < 0 {
			writeErr(w, http.StatusBadRequest, "每日请求数上限不能为负（0 = 不限制）")
			return
		}
		keys[idx].DailyRequestLimit = *req.DailyRequestLimit
	}
	if req.RPMLimit != nil {
		if *req.RPMLimit < 0 {
			writeErr(w, http.StatusBadRequest, "每分钟请求数上限不能为负（0 = 不限制）")
			return
		}
		keys[idx].RPMLimit = *req.RPMLimit
	}
	if req.MaxConcurrency != nil {
		if *req.MaxConcurrency < 0 {
			writeErr(w, http.StatusBadRequest, "并发上限不能为负（0 = 不限制）")
			return
		}
		keys[idx].MaxConcurrency = *req.MaxConcurrency
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

// keyUsage 返回该密钥今日已用 (tokens, requests)；未接线或尚无记录时为 0。
func (p *Panel) keyUsage(id string) (tokens, requests int64) {
	if p.cfg.KeyQuota == nil || id == "" {
		return 0, 0
	}
	return p.cfg.KeyQuota.Snapshot(id)
}

// keyLive 返回该密钥当前滑动一分钟请求数与在途并发；未接线时为 0。
func (p *Panel) keyLive(id string) (rpm, inflight int) {
	if p.cfg.KeyQuota == nil || id == "" {
		return 0, 0
	}
	return p.cfg.KeyQuota.Live(id)
}

// normalizeModelList 归一化可用模型白名单：去空白、去重、保序；空 = nil（不限制）。
// 条目形态（裸名 / cn: / global:）与 httpauth.Key.AllowsModel 同口径。
func normalizeModelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil
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
