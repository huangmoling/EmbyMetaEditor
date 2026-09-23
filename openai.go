package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode"
)

// openAIClient 是 OpenAI / 任意兼容中转的 chat/completions 翻译客户端。
//
// 只做一件事：把一段文本翻成简体中文。接口差异（官方 / 中转）靠 base_url 归一化吸收，
// 不在代码里写死任何厂商。
type openAIClient struct {
	cfg  OpenAIConfig
	http *http.Client
}

func newOpenAIClient(cfg OpenAIConfig, hc *http.Client) *openAIClient {
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	return &openAIClient{cfg: cfg, http: hc}
}

// normalizeChatURL 把用户填的「接口根」补齐成完整的 chat/completions 地址。
//
// 用户可能填：
//   - https://api.openai.com/v1              → .../v1/chat/completions
//   - https://api.openai.com                → .../v1/chat/completions
//   - https://api.gptgod.online/v1          → .../v1/chat/completions
//   - https://relay.example.com/v1/chat/completions → 原样（已带路径）
func normalizeChatURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	switch {
	case strings.HasSuffix(base, "/chat/completions"):
		return base
	case strings.HasSuffix(base, "/v1"):
		return base + "/chat/completions"
	default:
		return base + "/v1/chat/completions"
	}
}

// openAIChatRequest / Response 是 chat/completions 的最小可用结构。
type openAIChatRequest struct {
	Model       string              `json:"model"`
	Messages    []openAIChatMessage `json:"messages"`
	Temperature float64             `json:"temperature"`
	MaxTokens   int                 `json:"max_tokens,omitempty"`
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// sysPrompt 要求模型只返回译文本体，不要解释 / 引号 / 前缀。
const openAITranslateSys = "你是一个翻译器。把用户发来的文本翻译成简体中文。" +
	"只返回译文本身，不要任何解释、不要使用引号包裹、不要加「译文：」之类前缀。"

// Translate 把 text 翻成简体中文，返回译文。
func (o *openAIClient) Translate(ctx context.Context, text string) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}
	url := normalizeChatURL(o.cfg.BaseURL)
	if url == "" {
		return "", errOpenAINotConfigured
	}
	body, err := json.Marshal(openAIChatRequest{
		Model: o.cfg.Model,
		Messages: []openAIChatMessage{
			{Role: "system", Content: openAITranslateSys},
			{Role: "user", Content: text},
		},
		Temperature: 0.3,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(o.cfg.APIKey))
	req.Header.Set("User-Agent", cnUserAgent)

	resp, err := o.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		msg := out.ErrorMessage()
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return "", &openAIError{status: resp.StatusCode, msg: msg}
	}
	if len(out.Choices) == 0 {
		return "", errOpenAIEmpty
	}
	return cleanTranslation(out.Choices[0].Message.Content), nil
}

// Test 用一次极小的对话探测连通性：验证「地址可达 + Key 有效 + 模型存在」三件事。
// 返回 (ok, 人类可读信息)。它不修改任何配置，也不依赖 cfg.Enabled —— 即使翻译开关关着也能测。
func (o *openAIClient) Test(ctx context.Context) (bool, string) {
	url := normalizeChatURL(o.cfg.BaseURL)
	if url == "" {
		return false, "未填写接口地址（Base URL）"
	}
	if strings.TrimSpace(o.cfg.APIKey) == "" {
		return false, "未填写 API Key"
	}
	model := o.cfg.Model
	if model == "" {
		model = "gpt-4o-mini"
	}
	body, err := json.Marshal(openAIChatRequest{
		Model:       model,
		Messages:    []openAIChatMessage{{Role: "user", Content: "ping"}},
		Temperature: 0,
		MaxTokens:   1,
	})
	if err != nil {
		return false, "构造请求失败：" + err.Error()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, "构造请求失败：" + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(o.cfg.APIKey))
	req.Header.Set("User-Agent", cnUserAgent)

	resp, err := o.http.Do(req)
	if err != nil {
		return false, "连接失败：" + err.Error()
	}
	defer resp.Body.Close()
	var out openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, "响应解析失败（HTTP " + resp.Status + "）：" + err.Error()
	}
	if resp.StatusCode != http.StatusOK {
		msg := out.ErrorMessage()
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		return false, "接口返回错误（HTTP " + itoa(resp.StatusCode) + "）：" + msg
	}
	if len(out.Choices) == 0 {
		return false, "接口返回为空（未生成内容，可能是模型不可用）"
	}
	return true, "连通成功，模型可用：" + model
}

// cleanTranslation 去掉模型偶尔多返回的引号 / 空白 / 前缀。
func cleanTranslation(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "\"'\"")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "译文：")
	s = strings.TrimPrefix(s, "翻译：")
	s = strings.TrimSpace(s)
	return s
}

// needsTranslation 判断这段文本要不要翻成中文。
//
// 判定思路（够用即可，不追求 100% 精确）：
//   - 含日文假名（平假名 / 片假名）→ 日文，翻
//   - 含谚文 → 韩文，翻
//   - 含拉丁字母且不含任何汉字 → 英文 / 罗马音，翻
//   - 含汉字（多半已经是中文）→ 不翻
//
// 已知边界：纯汉字的日文标题（没有假名）会被当成中文跳过，不翻译。
// 这类在番号刮削里很少见，且翻错比不翻更糟，所以选择跳过。
func needsTranslation(s string) bool {
	if strings.TrimSpace(s) == "" {
		return false
	}
	var hasKana, hasHangul, hasCJK, hasLatin bool
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x30FF: // 平假名 + 片假名
			hasKana = true
		case r >= 0xAC00 && r <= 0xD7A3: // 谚文音节
			hasHangul = true
		case (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF): // CJK 统一表意
			hasCJK = true
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
			hasLatin = true
		}
	}
	if hasKana || hasHangul {
		return true
	}
	if hasLatin && !hasCJK {
		return true
	}
	return false
}

// isMostlyASCII 仅用于日志/诊断，判断一段文本是否基本是英文。
func isMostlyASCII(s string) bool {
	if s == "" {
		return false
	}
	var latin, other int
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == ' ' || r == '-' {
			latin++
		} else if !unicode.IsPunct(r) && !unicode.IsSpace(r) {
			other++
		}
	}
	return latin > 0 && other == 0
}

// translateMeta 是 App 层入口：读配置，对标题 / 简介各做「非中文才翻」，
// 错误时静默降级为原文（翻译是锦上添花，绝不该阻断刮削）。
// 返回翻译后的值，以及本次是否真的发起了翻译。
func (a *App) translateMeta(ctx context.Context, title, overview string) (string, string, bool) {
	cfg := a.store.Get()
	if !cfg.OpenAI.Ready() {
		return title, overview, false
	}
	cli := newOpenAIClient(cfg.OpenAI, newHTTPClient(cfg))
	var used bool
	if needsTranslation(title) {
		if t, err := cli.Translate(ctx, title); err == nil && t != "" {
			title = t
			used = true
		}
	}
	if needsTranslation(overview) {
		if o, err := cli.Translate(ctx, overview); err == nil && o != "" {
			overview = o
			used = true
		}
	}
	return title, overview, used
}

// ---- 错误类型 ----

var errOpenAINotConfigured = &openAIError{status: 0, msg: "OpenAI 未配置"}

var errOpenAIEmpty = &openAIError{status: 0, msg: "OpenAI 返回空结果"}

type openAIError struct {
	status int
	msg    string
}

func (e *openAIError) Error() string {
	if e.status != 0 {
		return "OpenAI 翻译失败（HTTP " + itoa(e.status) + "）：" + e.msg
	}
	return "OpenAI 翻译失败：" + e.msg
}

func (r *openAIChatResponse) ErrorMessage() string {
	if r == nil || r.Error == nil {
		return ""
	}
	return r.Error.Message
}
