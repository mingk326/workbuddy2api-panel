// busy.go 对外暴露官方客户端的「模型忙」预检端点：POST /v2/chat/busy。
//
// 背景（2026-10-21 真机抓包）：WorkBuddy 桌面客户端每次对话前都会调这个端点，
// body 形如 {"models":["deepseek-v4.1-flash", ...]}，上游回
// {"code":0,"msg":"OK","data":[{"model":"...","busy_level":"idle|busy|unknown"}]}，
// 客户端据此给模型选择器打「繁忙」标记。网关不实现这条路径时，把网关当后端的
// 客户端（自建面板 / 脚本 / 官方客户端）拿不到忙闲信息，只能靠 503 撞出来。
//
// 实现选择：本地按池状态算，不打上游。理由：
//   - 上游国际版同一路径 404，透传会让两个 realm 行为不一致；
//   - 网关自己就有权威的模型级冷却台账（modelCooldowns），比上游对聚合别名
//     一律回 unknown 更准；
//   - 不引入额外上游调用，不消耗额度、不放大风控面。
package server

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

// busyLevel 取值与官方一致：idle / busy / unknown。
const (
	busyLevelIdle    = "idle"
	busyLevelBusy    = "busy"
	busyLevelUnknown = "unknown"
)

// chatBusy 处理 POST /v2/chat/busy。
//
// 语义（每个请求模型映射成 busy_level）：
//   - busy：该模型在请求所属域的全部可用账号上都处于模型级冷却；
//   - idle：还有账号能服务该模型；
//   - unknown：模型不在网关当前服务目录里（拼错 / 未适配 / 被隐藏），或池为空。
//
// 未知模型回 unknown 而不是 idle：与官方口径一致，且不会让客户端误以为可选。
func (h *Handler) chatBusy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	out := make([]map[string]any, 0, len(req.Models))
	known := h.servableModelSet()
	// 密钥级模型白名单：名单外模型按 unknown 返回（不暴露池状态，也不让
	// 客户端把它当成可选模型）。未鉴权/名单为空 = 不限制。
	key, hasKey := authKeyFrom(r.Context())
	for _, m := range req.Models {
		realm, bare := resolveModel(m)
		lvl := busyLevelUnknown
		allowed := !hasKey || key.AllowsModel(realm, bare)
		if allowed {
			if _, ok := known[bare]; ok {
				if h.cfg.Pool != nil && h.cfg.Pool.ModelBlockedForRealm(bare, realm).Blocked {
					lvl = busyLevelBusy
				} else {
					lvl = busyLevelIdle
				}
			}
		}
		out = append(out, map[string]any{"model": m, "busy_level": lvl})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"code":      0,
		"msg":       "OK",
		"requestId": session.NewMessageID(),
		"data":      out,
	})
}

// servableModelSet 返回网关当前对外提供的模型 id 集合（裸名，不含 realm 前缀）。
//
// 取 /v1/models 同一份动态目录缓存（modelList 内部已按 hidden_models 过滤）；
// 缓存为空（还没探测过 / 探测失败）时回空集，调用方据此把模型判为 unknown
// ——不编造「这个模型我们有」。
func (h *Handler) servableModelSet() map[string]struct{} {
	infos := h.modelList()
	set := make(map[string]struct{}, len(infos))
	for _, it := range infos {
		id, _ := it["id"].(string)
		if id == "" {
			continue
		}
		_, bare := resolveModel(id)
		set[bare] = struct{}{}
	}
	return set
}
