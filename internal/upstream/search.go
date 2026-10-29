package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"workbuddy2api/internal/auth"
)

// searchPath 是上游联网搜索端点，相对 chatBase。
//
// 它**不套** {code,msg,data} 信封：实测返回
// {"query","type","provider","results":[{"title","url","snippet"}]}，
// 所以这里自己发原始 JSON，不复用 doJSON 的信封解包。
const searchPath = "/agenttool/v1/search"

// defaultSearchMaxResults 上游默认返回条数。
const defaultSearchMaxResults = 10

// SearchResult 一条搜索命中。字段名与上游一致，缺字段时留空。
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// searchResponse 上游响应体里我们关心的部分。
type searchResponse struct {
	Results []SearchResult `json:"results"`
}

// Search 用指定账号的凭据与出口线路执行一次联网搜索。
//
// 走 BillingHeaders（Bearer + 客户端指纹头）与 clientFor(a, false)：后者按账号的
// ProxyName 选出口，与聊天/计费请求同一条线路，避免同一个号从两个 IP 打上游。
func (c *Client) Search(a *auth.Auth, query string, maxResults int) ([]SearchResult, error) {
	if maxResults <= 0 {
		maxResults = defaultSearchMaxResults
	}
	payload, err := json.Marshal(map[string]any{
		"query":       query,
		"max_results": maxResults,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+searchPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	client, err := c.clientFor(a, false)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	var out searchResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse failed: %w (body: %s)", err, truncate(string(raw), 120))
	}
	return out.Results, nil
}
