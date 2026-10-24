// toggle.go 「(账号 × 模型) 手动开关」：运维显式指定某账号禁用某模型。
//
// 定位：这是**人工意图**，与自动机制（6004 限流 / 11102 无此模型）语义正交：
//
//	机制              触发者    持久性        解除方式
//	modelCooldowns    上游      临时（过期删）  到期自动 / 成功清除
//	modelOff（本文件）运维      永久（显式）    **仅手动解**
//
// **手动优先**：手动关掉的组合，自动恢复也不能打开——否则「我关了它，一次自动
// 恢复又开了」会让人无法信任这个开关。
//
// 存储：**只存被关闭的组合**（稀疏），不存全矩阵。
package pool

import (
	"sort"
	"time"
)

// modelOff 一条手动关闭记录（同时用于内存与 state.json）。
type modelOff struct {
	// Since 关闭时刻（展示用）。
	Since time.Time `json:"since"`
	// Note 关闭原因（运维备注，可选）。
	Note string `json:"note,omitempty"`
}

// SetModelOff 手动关闭某 (账号, 模型) 组合。
//
// model 用**裸名**（不含 cn:/global: 前缀）——前缀是网关侧路由协议，且同一账号
// 的域固定，带前缀存会让「模型改名/换域」时对不上。调用方负责剥前缀。
func (p *Pool) SetModelOff(uid, model, note string) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	if e.modelOff == nil {
		e.modelOff = map[string]modelOff{}
	}
	e.modelOff[model] = modelOff{Since: time.Now(), Note: note}
	p.dirty.Store(true)
	return true
}

// SetModelOn 手动启用（解除手动关闭）。返回是否确有变更。
func (p *Pool) SetModelOn(uid, model string) bool {
	if uid == "" || model == "" {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok || e.modelOff == nil {
		return false
	}
	if _, exists := e.modelOff[model]; !exists {
		return false
	}
	delete(e.modelOff, model)
	if len(e.modelOff) == 0 {
		e.modelOff = nil
	}
	p.dirty.Store(true)
	return true
}

// modelOffed 报告该 (账号, 模型) 是否被**手动**关闭。空 model / 未记录 → false。
func (e *entry) modelOffed(model string) bool {
	if model == "" || len(e.modelOff) == 0 {
		return false
	}
	_, ok := e.modelOff[model]
	return ok
}

// ModelOffEntry 面板展示用：一条手动关闭记录。
type ModelOffEntry struct {
	Model string `json:"model"`
	Since string `json:"since"` // 01-02 15:04
	Note  string `json:"note,omitempty"`
}

// modelOffEntries 把内存表转成稳定的展示列表（调用方持锁）。
func modelOffEntries(m map[string]modelOff) []ModelOffEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]ModelOffEntry, 0, len(m))
	for model, o := range m {
		out = append(out, ModelOffEntry{Model: model, Since: o.Since.Format("01-02 15:04"), Note: o.Note})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// ModelOffOf 返回某账号手动关闭的模型清单（按模型名升序，输出稳定）。
func (p *Pool) ModelOffOf(uid string) []ModelOffEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return nil
	}
	return modelOffEntries(e.modelOff)
}
