// images.go 文生图 / 图生图：POST {chatBase}/v2/images/generations 与 /v2/images/edits。
//
// 实测（带池内真实 token）：上游只有这两条图像路由——/v1/images/generations、
// /v2/image/generations、/v2/plugin/images/generations、/v2/chat/images/generations 全
// `{"error_msg":"404 Route Not Found"}`。请求 {model,prompt,n,size}：size 生效（默认
// 1536x1536）、n 被忽略（恒 1 张）、未知键被忽略；回程信封 data 已是 OpenAI 形状
// {created,data:[{url}],usage{...,credit}}，图是签名 COS 直链（约 24h 有效）——网关只
// 拆信封，不转存字节。edits 多一个 image（data URL 数组）。
//
// 模型名按 realm 不同，**计费模型也完全不同**（2026-10-18 复测）：
//
//	cn     hunyuan-image-alpha   每张固定 5.71 credit（与尺寸/token/张数无关）
//	global gpt-image-2.5-sunburst 按**输出图像 token** 计费：credit = tokens / 7300
//
// 目录里出现的媒体模型不一定在本域有出图路由——实测 global 目录同样列着
// hunyuan-image-alpha，但国际侧调它必回 400 code=14401「route config not found」。
package upstream

import (
	"encoding/json"
	"net/http"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// 图像通道（chatBase 域，与 growth/report 同 base 同头）。
const (
	imagesPath = "/v2/images/generations"
	editsPath  = "/v2/images/edits"
)

// ImageGenerations 文生图，返回上游 data 对象（OpenAI 形状，可直接透出）。
// 复用 growthJSON：同 chatBase、同 BillingHeaders、同信封解码与 *Error 语义。
func (c *Client) ImageGenerations(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, imagesPath, body)
}

// ImageEdits 图生图（image 为 data URL 数组），同样返回 OpenAI 形状的 data。
func (c *Client) ImageEdits(a *auth.Auth, body any) (json.RawMessage, error) {
	return c.growthJSON(a, http.MethodPost, editsPath, body)
}

// imageRouteRealm 已实测的「图像模型 → 有路由的域」。
//
// 为什么需要：列进 /v1/models 等于引诱客户端选一个必然失败的模型（客户端枚举列表
// 即踩）。未知模型照旧列出（不猜）。
var imageRouteRealm = map[string]string{
	"hunyuan-image-alpha":    "cn",
	"gpt-image-2.5-sunburst": "global",
}

// ImageModelRoutable 报告该图像模型在给定域是否有路由（未知模型视为可路由，不猜）。
func ImageModelRoutable(model, realm string) bool {
	if r, known := imageRouteRealm[model]; known {
		return r == realm
	}
	return true
}
