// modeloff.go 面板的「账号 × 模型」手动开关接口。
//
// 语义见 pool/toggle.go：手动关闭永久生效、手动优先（自动恢复不能重新打开）。
// 模型名统一按裸名存储（面板允许带 cn:/global: 前缀输入，这里剥掉）。
package panel

import (
	"encoding/json"
	"net/http"
	"strings"
)

type modelOffReq struct {
	Model string `json:"model"`
	Note  string `json:"note"`
}

// bareModelName 剥掉合法的 cn:/global: 前缀（与 server.resolveModel 的前缀规则对称）。
func bareModelName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i > 0 {
		switch s[:i] {
		case "cn", "global":
			s = s[i+1:]
		}
	}
	return s
}

func (p *Panel) accountModelOff(w http.ResponseWriter, r *http.Request) {
	var req modelOffReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	model := bareModelName(req.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	uid := r.PathValue("uid")
	if !p.cfg.Pool.SetModelOff(uid, model, strings.TrimSpace(req.Note)) {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": model, "off": true})
}

func (p *Panel) accountModelOn(w http.ResponseWriter, r *http.Request) {
	var req modelOffReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	model := bareModelName(req.Model)
	if model == "" {
		writeErr(w, http.StatusBadRequest, "model is required")
		return
	}
	uid := r.PathValue("uid")
	if !p.cfg.Pool.SetModelOn(uid, model) {
		// 幂等：本来就没关也视为成功（面板全量提交时不必区分）。
		if _, ok := p.cfg.Pool.Status(uid); !ok {
			writeErr(w, http.StatusNotFound, "account not found")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "model": model, "off": false})
}
