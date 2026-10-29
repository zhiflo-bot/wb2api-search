package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"workbuddy2api/internal/upstream"
)

// POST /v1/search/messages —— 把上游的 agenttool 联网搜索包成 Anthropic Messages
// 形态（`web_search_20250305` 的结果块），这样 dsh 的 web-search-deepseek provider
// 可以零改动接入：它往 `{baseURL}/messages` POST，从 `web_search_tool_result` 块里
// 取 `web_search_result` 的 url/title。
//
// 与 /v1/chat/completions 的关系：共用同一套鉴权（withAuth）、选号（Pool）、在途
// 名额（Acquire/Release）与出口线路（账号的 ProxyName）。区别是不做流式、不做
// 会话粘性、不写用量账本 —— 搜索不消耗模型 token，没有可记的用量。
//
// 请求体两种形态都收：
//   - {"query":"...","max_results":10}                       直接给查询词
//   - Anthropic Messages 形态（{"model","tools","messages"}）  取最后一条 user 文本
const searchMessagesPath = "/v1/search/messages"

// searchQueryPrefix dsh 的 provider 会这样包一句提示词；剥掉它才是真正的查询词。
const searchQueryPrefix = "perform a web search for the query:"

// searchRequest 请求体里我们认识的字段（两种形态的并集）。
type searchRequest struct {
	Query      string            `json:"query"`
	MaxResults int               `json:"max_results"`
	MaxResultsCamel int          `json:"maxResults"`
	Model      string            `json:"model"`
	Messages   []json.RawMessage `json:"messages"`
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

	// 选号轮转：搜索失败换号重试，与聊天同口径（MaxRotate 次）。
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
		results, err := h.cfg.Upstream.Search(acct, query, maxResults)
		h.cfg.Pool.Release(acct.UID)
		if err != nil {
			lastErr = err
			// 这里**不**写冷却：搜索失败（尤其是请求级 4xx）不代表账号不能聊天，
			// 罚号会把一条搜索错误放大成聊天 503。换号重试已经足够。
			continue
		}
		writeSearchMessages(w, req.Model, results)
		return
	}

	status := http.StatusServiceUnavailable
	msg := "no usable account for search"
	if lastErr != nil {
		var ue *upstream.Error
		if errors.As(lastErr, &ue) && ue.Status > 0 {
			status = http.StatusBadGateway
			msg = ue.Error()
		} else {
			status = http.StatusBadGateway
			msg = lastErr.Error()
		}
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
func writeSearchMessages(w http.ResponseWriter, model string, results []upstream.SearchResult) {
	items := make([]map[string]any, 0, len(results))
	for _, r := range results {
		if r.URL == "" {
			continue
		}
		item := map[string]any{"type": "web_search_result", "url": r.URL}
		if r.Title != "" {
			item["title"] = r.Title
		}
		// 上游给的是 snippet；Anthropic 的结果块没有 snippet 字段，但下游渲染会忽略
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
