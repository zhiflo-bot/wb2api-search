package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// POST /v1/search/messages —— 把上游 agenttool 联网搜索包成 Anthropic Messages
// 形态（`web_search_20250305` 的结果块），供 dsh 的 web-search-deepseek provider
// 直接接入：它往 `{baseURL}/messages` POST，从 `web_search_tool_result` 块里取
// `web_search_result` 的 url/title。
//
// 上游自 v2.11.15 起自带 `POST /v1/tools/search`（原样透传 agenttool 信封）和
// `Client.AgentToolSearch`。这里**复用它的上游调用**，只做两件它不做的事：
// 解析 Anthropic 形态的请求、把结果包成 Anthropic 的结果块。于是我们对上游的
// diff 只剩本文件 + handler.go 的一行路由。
//
// 与 /v1/chat/completions 共用鉴权（withAuth）、选号（Pool）与在途名额；不做
// 会话粘性（搜索无上下文，粘住单号会把该号的搜索日配额打穿）。
const searchMessagesPath = "/v1/search/messages"

// searchQueryPrefix dsh 的 provider 会这样包一句提示词；剥掉它才是真正的查询词。
const searchQueryPrefix = "perform a web search for the query:"

// searchRequest 请求体里我们认识的字段（两种形态的并集）。
type searchRequest struct {
	Query           string            `json:"query"`
	MaxResults      int               `json:"max_results"`
	MaxResultsCamel int               `json:"maxResults"`
	Model           string            `json:"model"`
	Messages        []json.RawMessage `json:"messages"`
}

// searchBlock Anthropic content block 的最小形态：只要 text 就够了。
type searchBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicMessageContent 一条 messages 元素的 content：字符串或 block 数组。
type anthropicMessageContent struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// searchMessages 处理联网搜索请求。
func (h *Handler) searchMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	var req searchRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body")
		return
	}
	query := strings.TrimSpace(req.Query)
	if query == "" {
		query = queryFromMessages(req.Messages)
	}
	if query == "" {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "no search query in request body")
		return
	}
	maxResults := req.MaxResults
	if maxResults <= 0 {
		maxResults = req.MaxResultsCamel
	}

	// 选号轮转：与 chat 同口径（MaxRotate 次），每轮纯轮换、不粘性。
	tried := map[string]bool{}
	var lastErr error
	for i := 0; i < h.cfg.MaxRotate; i++ {
		acct := h.cfg.Pool.PickExcludingForRealm(tried, "", "cn")
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			continue
		}
		resp, err := h.cfg.Upstream.AgentToolSearch(r.Context(), acct, upstream.AgentToolSearchRequest{
			Query:      query,
			Type:       "text2text",
			MaxResults: maxResults,
		})
		h.cfg.Pool.Release(acct.UID)
		if err != nil {
			lastErr = err
			// 搜索失败（尤其请求级 4xx）不代表账号不能聊天，这里**不写冷却**，
			// 只换号重试。配额类（15001）的冷却由上游 /v1/tools/search 负责。
			continue
		}
		if resp.Code != 0 {
			lastErr = &upstream.Error{Status: http.StatusBadGateway, Msg: resp.Msg}
			continue
		}
		writeSearchMessages(w, req.Model, resp.Results)
		return
	}

	status := http.StatusServiceUnavailable
	msg := "no usable account for search"
	if lastErr != nil {
		status = http.StatusBadGateway
		msg = lastErr.Error()
	}
	writeAnthropicError(w, status, "api_error", msg)
}

// queryFromMessages 从 Anthropic Messages 形态里取最后一条 user 文本。
func queryFromMessages(raw []json.RawMessage) string {
	for i := len(raw) - 1; i >= 0; i-- {
		var m anthropicMessageContent
		if err := json.Unmarshal(raw[i], &m); err != nil || m.Role != "user" {
			continue
		}
		text := textOfContent(m.Content)
		if text == "" {
			continue
		}
		lower := strings.ToLower(text)
		if idx := strings.Index(lower, searchQueryPrefix); idx >= 0 {
			text = text[idx+len(searchQueryPrefix):]
		}
		if q := strings.TrimSpace(text); q != "" {
			return q
		}
	}
	return ""
}

// textOfContent 把 content 归一成纯文本：字符串原样，block 数组拼 text 块。
func textOfContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return plain
	}
	var blocks []searchBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type != "text" || b.Text == "" {
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(b.Text)
	}
	return sb.String()
}

// writeSearchMessages 输出 Anthropic Messages 形态的搜索结果。
func writeSearchMessages(w http.ResponseWriter, model string, results []upstream.AgentToolSearchResult) {
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if r.URL == "" {
			continue
		}
		item := map[string]any{"type": "web_search_result", "url": r.URL}
		if r.Title != "" {
			item["title"] = r.Title
		}
		// 上游给的是 snippet；Anthropic 的结果块没有该字段，但下游渲染会忽略
		// 未知键，带上它比丢掉信息更好。
		if r.Snippet != "" {
			item["snippet"] = r.Snippet
		}
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":    "msg_" + randomHex(12),
		"type":  "message",
		"role":  "assistant",
		"model": model,
		"content": []map[string]any{{
			"type":        "web_search_tool_result",
			"tool_use_id": "srvtoolu_" + randomHex(12),
			"content":     items,
		}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 0, "output_tokens": 0},
	})
}

// writeAnthropicError 输出 Anthropic 形态的错误体。
func writeAnthropicError(w http.ResponseWriter, status int, kind, msg string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": msg,
		},
	})
}

// randomHex 生成 n 字节的随机十六进制串，用于消息 id。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
