// messages.go Anthropic Messages 入站：POST /v1/messages 与 /messages ↔ 现有 chatCompletions。
//
// 移植自 misakano7545 分支，并按本仓库的实际 API 做了裁剪：
//   - 不存会话；客户端每轮带全量 messages；上游 reasoning_content 无 signature，
//     作为 thinking 块下发展示；客户端回传的 thinking 块直接丢弃（不触发签名校验）。
//   - top_k / cache_control / context_management / betas / count_tokens 上游 chat
//     没有对应物，丢掉。
//   - 服务端 web_search 工具由网关代执行（searchtool.go）：探测→检索→结果注入，
//     客户端看到的仍是正常 Anthropic SSE 流。
//   - usage 语义对齐 relaykit：input_tokens 不含缓存读/写（Claude 口径），截断
//     映射 max_tokens，内容过滤映射 refusal。
//
// 路由注册（由 NewHandler 调用方添加，本文件不修改 handler.go）：
//
//	h.mux.HandleFunc("POST /v1/messages", h.withAnthropicAuth(h.messages))
//	h.mux.HandleFunc("POST /messages", h.withAnthropicAuth(h.messages))
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// withAnthropicAuth Anthropic 风格鉴权：官方 SDK 用 x-api-key 而不是 Bearer。
// 空 Authorization 时把 x-api-key 提升成 Bearer，再走同一套常量时间比较。
func (h *Handler) withAnthropicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if k := r.Header.Get("x-api-key"); k != "" {
				r.Header.Set("Authorization", "Bearer "+k)
			}
		}
		key, ok := h.authenticate(r)
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
			return
		}
		noteTraceKey(r, key)
		next(w, withAuthKey(r, key))
	}
}

// messages 把 Anthropic Messages 请求转成 chat_completions 后复用既有处理链，
// 再用 messagesWriter 把上游的 OpenAI 形状（JSON 或 SSE）转回 Anthropic 形状。
func (h *Handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	chat, srv, err := messagesToChat(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 客户端声明了 web_search 服务端工具 → 网关侧执行（探测→检索→结果注入）；
	// 注入失败不改语义：searchAugment 返回 nil body 时按普通对话继续。
	var prelude []any
	if len(srv) > 0 {
		if augmented, blocks := h.searchAugment(r.Context(), chat, srv[0]); augmented != nil {
			chat, prelude = augmented, blocks
		}
	}
	var peek struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(chat, &peek)
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(chat))
	r2.ContentLength = int64(len(chat))
	rw := &messagesWriter{ResponseWriter: w, stream: peek.Stream, prelude: prelude}
	defer rw.finish()
	h.chatCompletions(rw, r2)
}

func writeAnthropicError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": typ, "message": msg},
	})
}

// messagesToChat 把 Anthropic Messages 请求体转成 OpenAI chat_completions 请求体。
// 只搬运上游认识的字段；未知/无对应物的字段静默丢弃（见文件头注释）。
// 第三个返回值是客户端声明且本网关有执行器的服务端工具（web_search，见 searchtool.go）。
func messagesToChat(src []byte) ([]byte, []serverSearchTool, error) {
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return nil, nil, err
	}
	out := map[string]any{}
	for _, k := range []string{"model", "stream", "temperature", "top_p", "max_tokens"} {
		if v, ok := obj[k]; ok {
			out[k] = v
		}
	}
	if v, ok := obj["stop_sequences"]; ok {
		out["stop"] = v
	}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if u, ok := md["user_id"]; ok {
			out["user"] = u
		}
	}
	switch tc := obj["tool_choice"].(type) {
	case string:
		out["tool_choice"] = tc
	case map[string]any:
		if c, ok := mapAnthropicToolChoice(tc); ok {
			out["tool_choice"] = c
		}
		if dis, ok := tc["disable_parallel_tool_use"].(bool); ok && dis {
			out["parallel_tool_calls"] = false
		}
	}
	var skipped []string
	var srv []serverSearchTool
	if raw, ok := obj["tools"].([]any); ok {
		tools, skip, hosted := anthropicTools(raw)
		if len(tools) > 0 {
			out["tools"] = tools
		}
		skipped = skip
		srv = hosted
	}
	if e := effortFromMessages(obj); e != "" {
		out["reasoning_effort"] = e
	}
	msgs := []any{}
	if sys := systemText(obj["system"]); sys != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": sys})
	}
	if note := skippedToolsNote(skipped); note != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": note})
	}
	if in, ok := obj["messages"].([]any); ok {
		msgs = append(msgs, convertAnthropicMessages(in)...)
	}
	out["messages"] = msgs
	b, err := json.Marshal(out)
	if err != nil {
		return nil, nil, err
	}
	return b, srv, nil
}

// effortFromMessages 把 Messages 的思考档收成上游已有的 reasoning_effort。
// output_config.effort 优先；旧式 budget_tokens 按 Hermes 的档位表折回。
// adaptive / enabled 没带档位时不发明档位，交给上游默认。disabled 显式 none。
func effortFromMessages(obj map[string]any) string {
	if oc, ok := obj["output_config"].(map[string]any); ok {
		if e := asString(oc["effort"]); e != "" {
			return e
		}
	}
	th, _ := obj["thinking"].(map[string]any)
	if th == nil {
		return ""
	}
	switch strings.ToLower(asString(th["type"])) {
	case "disabled":
		return "none"
	case "enabled":
		n, ok := th["budget_tokens"].(float64)
		if !ok || n <= 0 {
			return ""
		}
		switch {
		case n >= 32000:
			return "xhigh"
		case n >= 16000:
			return "high"
		case n >= 8000:
			return "medium"
		default:
			return "low"
		}
	default:
		return ""
	}
}

func skippedToolsNote(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return "These Anthropic server tools are not available here: " + strings.Join(names, ", ") + ". Do not call them."
}

func systemText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for i, p := range t {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			text := asString(m["text"])
			if text == "" {
				continue
			}
			if i > 0 && b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(text)
		}
		return b.String()
	default:
		return ""
	}
}

func mapAnthropicToolChoice(m map[string]any) (any, bool) {
	switch asString(m["type"]) {
	case "auto":
		return "auto", true
	case "any":
		return "required", true
	case "none":
		return "none", true
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": asString(m["name"])}}, true
	default:
		return nil, false
	}
}

// anthropicTools 分三类：客户端自己执行的工具保留、本网关有执行器的服务端工具交给
// 探测阶段（见 searchtool.go）、其余服务端工具返回名字供 system 说明。
// 带 input_schema 的（含 type=custom / 缺 type）原样变成 function。
func anthropicTools(tools []any) (out []any, skipped []string, srv []serverSearchTool) {
	out = make([]any, 0, len(tools))
	for _, t := range tools {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := m["function"].(map[string]any); ok {
			out = append(out, m)
			continue
		}
		if _, ok := m["input_schema"]; !ok {
			if isWebSearchTool(m) {
				srv = append(srv, serverSearchTool{Name: asString(m["name"]), MaxUses: int(asNum(m["max_uses"]))})
				continue
			}
			if n := toolLabel(m); n != "" {
				skipped = append(skipped, n)
			}
			continue
		}
		fn := map[string]any{"name": asString(m["name"])}
		if d, ok := m["description"]; ok {
			fn["description"] = d
		}
		fn["parameters"] = m["input_schema"]
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out, skipped, srv
}

// isWebSearchTool 官方联网检索服务端工具：type 以 web_search 开头
// （web_search_20250305 / web_search_20250910 …）或名字恰为 web_search。名字为空时
// 无法构造 function 声明，一律当不可执行。
func isWebSearchTool(m map[string]any) bool {
	if asString(m["name"]) == "" {
		return false
	}
	if t := asString(m["type"]); t != "" {
		return strings.HasPrefix(t, webSearchToolName)
	}
	return asString(m["name"]) == webSearchToolName
}

func toolLabel(m map[string]any) string {
	if n := asString(m["name"]); n != "" {
		return n
	}
	return asString(m["type"])
}

func convertAnthropicMessages(in []any) []any {
	var out, toolImgs []any
	// 工具结果里的图片另起一条 user 消息（Chat 的 tool 消息不许带图），且必须等
	// 这批工具结果全部落地 —— 插在 assistant.tool_calls 与它的 tool 消息之间会
	// 打断配对。
	flushImgs := func() {
		if len(toolImgs) == 0 {
			return
		}
		out = append(out, map[string]any{
			"role":    "user",
			"content": append([]any{map[string]any{"type": "text", "text": messagesToolImagePlaceholder}}, toolImgs...),
		})
		toolImgs = nil
	}
	for _, it := range in {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		role := asString(m["role"])
		if role == "" {
			role = "user"
		}
		if blocks, isBlocks := m["content"].([]any); !isBlocks || !hasToolResult(blocks) {
			flushImgs()
		}
		switch c := m["content"].(type) {
		case string:
			out = append(out, map[string]any{"role": role, "content": c})
		case []any:
			msgs, imgs := blocksToMessages(role, c)
			out = append(out, msgs...)
			toolImgs = append(toolImgs, imgs...)
		default:
			if s := asString(m["content"]); s != "" {
				out = append(out, map[string]any{"role": role, "content": s})
			}
		}
	}
	flushImgs()
	return out
}

// hasToolResult 本条消息是否含 tool_result 块（图片消息要等这类消息处理完再落地）。
func hasToolResult(blocks []any) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && asString(m["type"]) == "tool_result" {
			return true
		}
	}
	return false
}

func blocksToMessages(role string, blocks []any) (msgs []any, imgs []any) {
	var toolCalls, toolMsgs, parts []any
	var texts []string
	hasNonText := false
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch asString(m["type"]) {
		case "thinking", "redacted_thinking":
			continue
		case "tool_use", "server_tool_use":
			toolCalls = append(toolCalls, map[string]any{
				"id":   asString(m["id"]),
				"type": "function",
				"function": map[string]any{
					"name":      asString(m["name"]),
					"arguments": mustJSON(m["input"]),
				},
			})
		case "tool_result":
			txt, toolImgs := splitToolResultImages(m["content"])
			if e, ok := m["is_error"].(bool); ok && e {
				txt = "error: " + txt
			}
			toolMsgs = append(toolMsgs, map[string]any{
				"role":         "tool",
				"tool_call_id": asString(m["tool_use_id"]),
				"content":      txt,
			})
			imgs = append(imgs, toolImgs...)
		case "image":
			hasNonText = true
			parts = append(parts, imagePart(m))
		case "document":
			hasNonText = true
			parts = append(parts, map[string]any{"type": "text", "text": documentText(m)})
		default:
			t := blockText(m)
			texts = append(texts, t)
			parts = append(parts, map[string]any{"type": "text", "text": t})
		}
	}
	out := append([]any{}, toolMsgs...)
	if len(toolCalls) == 0 && len(texts) == 0 && !hasNonText {
		return out, imgs
	}
	msg := map[string]any{"role": role}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	if hasNonText {
		msg["content"] = parts
	} else if len(texts) > 0 {
		msg["content"] = strings.Join(texts, "")
	} else {
		msg["content"] = ""
	}
	return append(out, msg), imgs
}

// splitToolResultImages 拆开 tool_result 的正文与图片。图片不能留在 tool 消息里
// （Chat 的 tool 消息不许带图），也不能继续拼进正文 —— 拼进去等于把 base64 当正文
// 分词。文本部分的取法与 contentToString 一致。
func splitToolResultImages(v any) (string, []any) {
	blocks, ok := v.([]any)
	if !ok {
		return contentToString(v), nil
	}
	var texts []string
	var imgs []any
	for _, b := range blocks {
		m, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch asString(m["type"]) {
		case "image":
			imgs = append(imgs, imagePart(m))
		case "document":
			texts = append(texts, documentText(m))
		default:
			texts = append(texts, blockText(m))
		}
	}
	return strings.Join(texts, ""), imgs
}

func imagePart(m map[string]any) map[string]any {
	url := ""
	if src, ok := m["source"].(map[string]any); ok {
		switch asString(src["type"]) {
		case "base64":
			url = "data:" + asString(src["media_type"]) + ";base64," + asString(src["data"])
		default:
			url = asString(src["url"])
		}
	}
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
}

func blockText(m map[string]any) string {
	if t := asString(m["text"]); t != "" || m["text"] != nil {
		return t
	}
	if t := asString(m["thinking"]); t != "" {
		return t
	}
	if c, ok := m["content"]; ok {
		return toolResultContent(c, nil)
	}
	return ""
}

func documentText(m map[string]any) string {
	title := asString(m["title"])
	src, _ := m["source"].(map[string]any)
	var body string
	if src != nil {
		switch asString(src["type"]) {
		case "text":
			body = asString(src["data"])
		case "content":
			body = toolResultContent(src["content"], nil)
		case "base64":
			body = "[document " + asString(src["media_type"]) + " omitted]"
		case "url":
			body = asString(src["url"])
		}
	}
	if title != "" {
		return title + "\n" + body
	}
	return body
}

func toolResultContent(v any, isError any) string {
	s := contentToString(v)
	if b, ok := isError.(bool); ok && b {
		return "error: " + s
	}
	return s
}

func contentToString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch asString(m["type"]) {
			case "image":
				img := imagePart(m)
				if u, ok := img["image_url"].(map[string]any); ok {
					b.WriteString(asString(u["url"]))
				}
			case "document":
				b.WriteString(documentText(m))
			default:
				b.WriteString(blockText(m))
			}
		}
		return b.String()
	case map[string]any:
		if _, ok := t["type"]; ok {
			return blockText(t)
		}
		return mustJSON(t)
	default:
		return asString(v)
	}
}

// ---------------------------------------------------------------------------
// 响应方向：OpenAI chat.completion → Anthropic message
// ---------------------------------------------------------------------------

func chatCompletionToMessage(obj map[string]any) map[string]any {
	id, _ := obj["id"].(string)
	model, _ := obj["model"].(string)
	var msg map[string]any
	fr := ""
	if chs, ok := obj["choices"].([]any); ok && len(chs) > 0 {
		if c, ok := chs[0].(map[string]any); ok {
			msg, _ = c["message"].(map[string]any)
			fr, _ = c["finish_reason"].(string)
		}
	}
	content := []any{}
	if msg != nil {
		// 上游 reasoning_content 无 signature，作为 thinking 块下发（Claude Code 展示用）；
		// 客户端回传的 thinking 块在 blocksToMessages 里被丢弃，不会触发签名校验。
		if rc, ok := msg["reasoning_content"].(string); ok && rc != "" {
			content = append(content, map[string]any{"type": "thinking", "thinking": rc})
		}
		if text := messageText(msg["content"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		if tcs, ok := msg["tool_calls"].([]any); ok {
			for _, tci := range tcs {
				tc, _ := tci.(map[string]any)
				if tc == nil {
					continue
				}
				fn, _ := tc["function"].(map[string]any)
				name, args := "", any(nil)
				if fn != nil {
					name = asString(fn["name"])
					args = fn["arguments"]
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    asString(tc["id"]),
					"name":  name,
					"input": parseToolInput(args),
				})
			}
		}
	}
	return map[string]any{
		"id":            msgID(id),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   anthropicStop(fr),
		"stop_sequence": nil,
		"usage":         anthropicUsage(obj["usage"]),
	}
}

func messageText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return asContentString(v)
}

func parseToolInput(v any) any {
	switch t := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return t
	case string:
		if t == "" {
			return map[string]any{}
		}
		var parsed any
		if json.Unmarshal([]byte(t), &parsed) != nil || parsed == nil {
			return map[string]any{"raw": t}
		}
		if _, ok := parsed.(map[string]any); ok {
			return parsed
		}
		return map[string]any{"value": parsed}
	default:
		return map[string]any{"value": t}
	}
}

func anthropicStop(fr string) string {
	switch fr {
	case "length", "max_tokens":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

func anthropicUsage(v any) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	u, _ := v.(map[string]any)
	if u == nil {
		return out
	}
	if n, ok := u["prompt_tokens"]; ok {
		out["input_tokens"] = n
	}
	if n, ok := u["completion_tokens"]; ok {
		out["output_tokens"] = n
	}
	// 上游真实命中数在 prompt_cache_hit_tokens（本仓库 upstream 已同时写
	// cache_read_input_tokens / cached_tokens 别名）。不映射的话 Claude 系客户端
	// 一律显示缓存读取 0。
	if n, ok := u["prompt_cache_hit_tokens"]; ok {
		out["cache_read_input_tokens"] = n
	}
	if n, ok := u["prompt_cache_write_tokens"]; ok {
		out["cache_creation_input_tokens"] = n
	}
	// Claude 口径：input_tokens 不含缓存读/写（单独字段计）。上游 prompt_tokens 是
	// 全量，不减的话 Claude Code 的上下文占用会被缓存量虚抬。
	sub := asNum(u["prompt_cache_hit_tokens"]) + asNum(u["prompt_cache_write_tokens"])
	if sub > 0 {
		in := asNum(out["input_tokens"]) - sub
		if in < 0 {
			in = 0
		}
		out["input_tokens"] = in
	}
	return out
}

func asNum(v any) float64 {
	f, _ := v.(float64)
	return f
}

func msgID(id string) string {
	if id == "" {
		id = "wb2api"
	}
	if strings.HasPrefix(id, "msg_") {
		return id
	}
	return "msg_" + id
}

func anthropicErrType(status int) string {
	switch status {
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

func anthropicErrorBody(status int, obj map[string]any) []byte {
	msg := "upstream error"
	if e, ok := obj["error"].(map[string]any); ok {
		if m := asString(e["message"]); m != "" {
			msg = m
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": anthropicErrType(status), "message": msg},
	})
	return raw
}

// ---------------------------------------------------------------------------
// messagesWriter：把 chatCompletions 写出的 OpenAI JSON / SSE 转成 Anthropic 形状
// ---------------------------------------------------------------------------

type messagesWriter struct {
	http.ResponseWriter
	stream  bool
	status  int
	hdrSent bool
	rest    []byte
	body    []byte
	x       anthState
	// prelude 服务端工具块（server_tool_use / web_search_tool_result，见 searchtool.go）：
	// 在 message_start 之后、正文块之前一次性下发（内容已完整，无需 delta 帧）。
	prelude []any
}

type anthState struct {
	started, failed, done bool
	id, model, stop       string
	thinkOpen             bool
	thinkIdx              int
	textOpen              bool
	textIdx, next         int
	tools                 map[int]*anthTool
	toolOrder             []int
	usage                 map[string]any
}

type anthTool struct {
	idx            int
	block          int
	id, name       string
	opened, closed bool
}

func (w *messagesWriter) Header() http.Header { return w.ResponseWriter.Header() }

// WriteHeader 只记录状态码，不立即下发：非流式要等 finish 时才能决定 JSON 形状，
// 流式则统一由 emit 提交 200。
func (w *messagesWriter) WriteHeader(code int) { w.status = code }

func (w *messagesWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *messagesWriter) Write(p []byte) (int, error) {
	if w.stream && strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
		w.rest = append(w.rest, p...)
		for {
			i := bytes.Index(w.rest, []byte("\n\n"))
			if i < 0 {
				break
			}
			frame := string(bytes.TrimSpace(w.rest[:i]))
			w.rest = w.rest[i+2:]
			if err := w.handleFrame(frame); err != nil {
				return len(p), err
			}
		}
		return len(p), nil
	}
	w.body = append(w.body, p...)
	return len(p), nil
}

func (w *messagesWriter) finish() {
	if w.stream {
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") || w.hdrSent {
			if w.x.started && !w.x.done && !w.x.failed {
				_ = w.closeAndStop()
			}
			return
		}
		_ = w.emitJSONError()
		return
	}
	w.flushJSON()
}

func (w *messagesWriter) flushJSON() {
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	if len(w.body) == 0 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	var obj map[string]any
	if json.Unmarshal(w.body, &obj) != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	if _, ok := obj["error"]; ok {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(anthropicErrorBody(status, obj))
		return
	}
	msg := chatCompletionToMessage(obj)
	if len(w.prelude) > 0 {
		if c, ok := msg["content"].([]any); ok {
			msg["content"] = append(append([]any{}, w.prelude...), c...)
			w.prelude = nil
		}
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		w.ResponseWriter.WriteHeader(status)
		_, _ = w.ResponseWriter.Write(w.body)
		return
	}
	w.ResponseWriter.WriteHeader(status)
	_, _ = w.ResponseWriter.Write(raw)
}

func (w *messagesWriter) emitJSONError() error {
	var obj map[string]any
	_ = json.Unmarshal(w.body, &obj)
	msg := "upstream error"
	if e, ok := obj["error"].(map[string]any); ok {
		if m := asString(e["message"]); m != "" {
			msg = m
		}
	}
	status := w.status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return w.emitError(msg, anthropicErrType(status))
}

func (w *messagesWriter) handleFrame(frame string) error {
	if w.x.failed || w.x.done {
		return nil
	}
	if strings.HasPrefix(frame, "data: [DONE]") {
		return w.closeAndStop()
	}
	payload, ok := strings.CutPrefix(frame, "data: ")
	if !ok {
		return nil
	}
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return nil
	}
	if errObj, ok := obj["error"]; ok {
		msg := "upstream error"
		if e, ok := errObj.(map[string]any); ok {
			if m := asString(e["message"]); m != "" {
				msg = m
			}
		}
		return w.emitError(msg, "api_error")
	}
	return w.handleChunk(obj)
}

func (w *messagesWriter) handleChunk(obj map[string]any) error {
	if err := w.ensureStart(obj); err != nil {
		return err
	}
	if u, ok := obj["usage"].(map[string]any); ok {
		w.x.usage = u
	}
	chs, _ := obj["choices"].([]any)
	if len(chs) == 0 {
		return nil
	}
	c, _ := chs[0].(map[string]any)
	if c == nil {
		return nil
	}
	if d, _ := c["delta"].(map[string]any); d != nil {
		// 上游 reasoning_content → thinking 块（Claude Code 会展示）。无 signature：
		// 客户端回传时被 blocksToMessages 丢掉，不会触发校验。
		if rc, ok := d["reasoning_content"].(string); ok && rc != "" {
			if err := w.ensureThinking(); err != nil {
				return err
			}
			if err := w.emit("content_block_delta", map[string]any{
				"index": w.x.thinkIdx,
				"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
			}); err != nil {
				return err
			}
		}
		if s, ok := d["content"].(string); ok && s != "" {
			if err := w.ensureText(); err != nil {
				return err
			}
			if err := w.emit("content_block_delta", map[string]any{
				"index": w.x.textIdx,
				"delta": map[string]any{"type": "text_delta", "text": s},
			}); err != nil {
				return err
			}
		}
		if tcs, ok := d["tool_calls"].([]any); ok {
			if err := w.onTools(tcs); err != nil {
				return err
			}
		}
	}
	if fr, ok := c["finish_reason"].(string); ok && fr != "" {
		w.x.stop = fr
	}
	return nil
}

func (w *messagesWriter) ensureStart(obj map[string]any) error {
	if w.x.started {
		return nil
	}
	w.x.started = true
	if obj != nil {
		w.x.id, _ = obj["id"].(string)
		w.x.model, _ = obj["model"].(string)
	}
	// usage 在最后一帧才到，message_start 的 input_tokens 固定 0；真实数在 message_delta。
	if err := w.emit("message_start", map[string]any{
		"message": map[string]any{
			"id":            msgID(w.x.id),
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         w.x.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}); err != nil {
		return err
	}
	return w.emitPrelude()
}

// emitPrelude 落地服务端工具块：每次都自带 start+stop（input/结果已是完整的，无需
// delta 帧），各自占用一个内容块下标。只发一次（发完清空）。
func (w *messagesWriter) emitPrelude() error {
	for _, block := range w.prelude {
		idx := w.x.next
		w.x.next++
		if err := w.emit("content_block_start", map[string]any{"index": idx, "content_block": block}); err != nil {
			return err
		}
		if err := w.emit("content_block_stop", map[string]any{"index": idx}); err != nil {
			return err
		}
	}
	w.prelude = nil
	return nil
}

func (w *messagesWriter) ensureThinking() error {
	if w.x.thinkOpen {
		return nil
	}
	if w.x.textOpen {
		if err := w.emit("content_block_stop", map[string]any{"index": w.x.textIdx}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	w.x.thinkIdx = w.x.next
	w.x.next++
	w.x.thinkOpen = true
	return w.emit("content_block_start", map[string]any{
		"index":         w.x.thinkIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

func (w *messagesWriter) closeThinking() error {
	if !w.x.thinkOpen {
		return nil
	}
	w.x.thinkOpen = false
	return w.emit("content_block_stop", map[string]any{"index": w.x.thinkIdx})
}

func (w *messagesWriter) ensureText() error {
	if err := w.closeThinking(); err != nil {
		return err
	}
	if w.x.textOpen {
		return nil
	}
	w.x.textIdx = w.x.next
	w.x.next++
	w.x.textOpen = true
	return w.emit("content_block_start", map[string]any{
		"index":         w.x.textIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (w *messagesWriter) onTools(tcs []any) error {
	if w.x.tools == nil {
		w.x.tools = map[int]*anthTool{}
	}
	for _, tci := range tcs {
		tc, _ := tci.(map[string]any)
		if tc == nil {
			continue
		}
		idx := 0
		if v, ok := tc["index"].(float64); ok {
			idx = int(v)
		}
		acc, ok := w.x.tools[idx]
		if !ok {
			acc = &anthTool{idx: idx}
			w.x.tools[idx] = acc
			w.x.toolOrder = append(w.x.toolOrder, idx)
		}
		if id := asString(tc["id"]); id != "" {
			acc.id = id
		}
		fn, _ := tc["function"].(map[string]any)
		args := ""
		if fn != nil {
			if n := asString(fn["name"]); n != "" {
				acc.name = n
			}
			args = asString(fn["arguments"])
		}
		// 无名调用不是合法 tool_use：块一旦开出，content_block_start 里的 name:"" 就
		// 定死了，客户端拿到也派发不了。等 name 到达再开块，无名调用直接丢弃。
		if !acc.opened && acc.name != "" && (acc.id != "" || args != "") {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
		if args != "" && acc.opened {
			if err := w.emit("content_block_delta", map[string]any{
				"index": acc.block,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *messagesWriter) openTool(acc *anthTool) error {
	if acc.opened {
		return nil
	}
	if err := w.closeThinking(); err != nil {
		return err
	}
	if w.x.textOpen {
		if err := w.emit("content_block_stop", map[string]any{"index": w.x.textIdx}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	acc.opened = true
	acc.block = w.x.next
	w.x.next++
	if acc.id == "" {
		acc.id = "toolu_" + asString(acc.idx)
	}
	return w.emit("content_block_start", map[string]any{
		"index": acc.block,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    acc.id,
			"name":  acc.name,
			"input": map[string]any{},
		},
	})
}

func (w *messagesWriter) closeAndStop() error {
	if w.x.done || w.x.failed {
		return nil
	}
	w.x.done = true
	if !w.x.started {
		if err := w.ensureStart(nil); err != nil {
			return err
		}
	}
	if err := w.closeThinking(); err != nil {
		return err
	}
	if w.x.textOpen {
		if err := w.emit("content_block_stop", map[string]any{"index": w.x.textIdx}); err != nil {
			return err
		}
		w.x.textOpen = false
	}
	for _, idx := range w.x.toolOrder {
		acc := w.x.tools[idx]
		if acc.name == "" {
			// 名字始终没到：不是合法 tool_use，连块都不开。
			continue
		}
		if !acc.opened {
			if err := w.openTool(acc); err != nil {
				return err
			}
		}
		if !acc.closed {
			if err := w.emit("content_block_stop", map[string]any{"index": acc.block}); err != nil {
				return err
			}
			acc.closed = true
		}
	}
	if err := w.emit("message_delta", map[string]any{
		"delta": map[string]any{"stop_reason": anthropicStop(w.x.stop), "stop_sequence": nil},
		"usage": anthropicUsage(w.x.usage),
	}); err != nil {
		return err
	}
	return w.emit("message_stop", map[string]any{})
}

func (w *messagesWriter) emitError(msg, typ string) error {
	if w.x.failed || w.x.done {
		return nil
	}
	w.x.failed = true
	w.x.done = true
	return w.emit("error", map[string]any{
		"error": map[string]any{"type": typ, "message": msg},
	})
}

func (w *messagesWriter) emit(event string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = event
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if !w.hdrSent {
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		// 不显式 WriteHeader：下面的 io.WriteString 会隐式提交 200。
		w.hdrSent = true
		w.status = http.StatusOK
	}
	if _, err := io.WriteString(w.ResponseWriter, "event: "+event+"\ndata: "+string(raw)+"\n\n"); err != nil {
		return err
	}
	w.Flush()
	return nil
}

// ---------------------------------------------------------------------------
// 本仓库缺失的通用小工具（源实现放在 responses.go，这里就地定义以免跨文件依赖）
// ---------------------------------------------------------------------------

// messagesToolImagePlaceholder 工具结果里的图片被挪进合成 user 消息时带的占位正文。
// 注：responses.go 已定义同语义的 toolImagePlaceholder（包级常量），此处用独立名字避免重名。
const messagesToolImagePlaceholder = "[image from tool result]"

// asString 把任意 JSON 值转成字符串：字符串原样、nil 空串、其余按 JSON 序列化。
// responses.go 的 Anthropic/Responses 适配共用本函数（那边不再重复定义）。
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case nil:
		return ""
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// asContentString 把 content（字符串或 text 块数组）拼成纯文本。
func asContentString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, p := range t {
			if m, ok := p.(map[string]any); ok {
				b.WriteString(asString(m["text"]))
			}
		}
		return b.String()
	default:
		return asString(v)
	}
}

func mustJSON(v any) string {
	if v == nil {
		return "{}"
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}
