package inspector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "local.sub2api/gpt-inspector/internal/pluginapi"
)

func newID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&15 | 64
	b[8] = b[8]&63 | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func timestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Server) listAccounts(ctx context.Context) ([]Account, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := s.host.ListAccounts(ctx, &pluginv1.ListAccountsRequest{Platform: "openai", AccountType: "oauth"})
	if err != nil {
		return nil, errors.New("读取宿主账号目录失败")
	}
	accounts := make([]Account, 0, len(r.Accounts))
	for _, a := range r.Accounts {
		if a.Id > 0 && a.Platform == "openai" && a.AccountType == "oauth" && !a.IsShadow && a.Status == "active" {
			accounts = append(accounts, Account{a.Id, a.Name, a.Schedulable})
		}
	}
	return accounts, nil
}
func (s *Server) identity(ctx context.Context, id int64) (*pluginv1.ResolveOutboundIdentityResponse, error) {
	accounts, err := s.listAccounts(ctx)
	if err != nil {
		return nil, err
	}
	found := false
	for _, a := range accounts {
		if a.ID == id {
			found = a.Schedulable
			break
		}
	}
	if !found {
		return nil, errors.New("账号不可用或当前暂停调度，请在账号页检查状态")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	i, err := s.host.ResolveOutboundIdentity(ctx, &pluginv1.ResolveOutboundIdentityRequest{AccountId: id})
	if err != nil {
		return nil, errors.New("获取账号临时出站身份失败")
	}
	if !i.Found || i.AccountId != id || i.Platform != "openai" || i.AccountType != "oauth" || i.Token == "" {
		return nil, errors.New("所选账号未提供可用的 OpenAI OAuth 身份")
	}
	return i, nil
}
func identityHeaders(i *pluginv1.ResolveOutboundIdentityResponse) http.Header {
	h := headersFromProto(i.Headers)
	h.Set("Authorization", "Bearer "+i.Token)
	h.Set("Accept-Encoding", "identity")
	return h
}
func (s *Server) fetchModels(ctx context.Context, id int64) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	i, err := s.identity(ctx, id)
	if err != nil {
		return nil, err
	}
	h := identityHeaders(i)
	h.Set("Accept", "application/json")
	u, _ := url.Parse(s.modelsURL)
	q := u.Query()
	q.Set("client_version", h.Get("Version"))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header = h
	t, err := s.pool.get(i.ProxyUrl)
	if err != nil {
		return nil, err
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		return nil, errors.New(networkError(ctx, err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024+1))
	if err != nil {
		return nil, errors.New("模型目录读取中断")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, upstreamError(resp.StatusCode, raw, i)
	}
	if len(raw) > 2*1024*1024 {
		return nil, errors.New("上游模型目录超过大小限制")
	}
	var catalog struct {
		Models []struct {
			Slug   string `json:"slug"`
			Name   string `json:"display_name"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, errors.New("上游未返回有效的 Codex 模型目录")
	}
	models := make([]Model, 0, len(catalog.Models))
	seen := map[string]bool{}
	for _, m := range catalog.Models {
		id := strings.TrimSpace(m.Slug)
		if id == "" || len(id) > 200 || seen[id] {
			continue
		}
		seen[id] = true
		x := Model{ID: id, Name: m.Name, Efforts: []string{}}
		levels := map[string]bool{}
		for _, l := range m.Levels {
			effort := strings.TrimSpace(l.Effort)
			if effort != "" && len(effort) <= 32 && !levels[effort] {
				levels[effort] = true
				x.Efforts = append(x.Efforts, effort)
			}
		}
		models = append(models, x)
	}
	if len(models) == 0 {
		return nil, errors.New("账号的 Codex 模型目录为空，请重试")
	}
	return models, nil
}

func requestBody(selection Selection, prompt Prompt, sessionID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": selection.Model, "stream": true, "store": false,
		"instructions": "请独立完成用户的任务。不要联网，不使用任何工具。",
		"input":        []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": prompt.Text}}}},
		"reasoning":    map[string]any{"effort": selection.Effort},
		"tools":        []any{}, "tool_choice": "none", "parallel_tool_calls": false,
		"prompt_cache_key": sessionID,
	})
	return body
}
func testRequestContext(parent context.Context, minutes int) (context.Context, context.CancelFunc) {
	if minutes == 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(minutes)*time.Minute)
}

const maxRequestAttempts = 3

func retryEmptyResponse(a Answer) bool {
	if a.Text != "" || a.Reasoning != "" {
		return false
	}
	return a.ErrorCode == "transport_error" || a.ErrorCode == "stream_interrupted" || a.ErrorCode == "stream_incomplete"
}

func (s *Server) ask(parent context.Context, selection Selection, prompt Prompt, sessionID string, onRetry func(int, string) error) (a Answer) {
	a.Prompt = prompt
	minutes := defaultTimeoutMinutes
	if selection.TimeoutMinutes != nil {
		minutes = *selection.TimeoutMinutes
	}
	ctx, cancel := testRequestContext(parent, minutes)
	defer func() {
		if parent.Err() != nil {
			a.ErrorCode = "canceled"
			a.Error = "测试已停止，已保留接收到的部分内容"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			a.ErrorCode = "timeout"
			a.Error = fmt.Sprintf("达到单题 %d 分钟超时，已保留接收到的部分内容；可增加时长或选择不限时", minutes)
		}
		cancel()
	}()
	var attempts []RequestAttempt
	for number := 1; number <= maxRequestAttempts; number++ {
		if number > 1 {
			// All attempts share the original deadline. Stopping or clearing also
			// interrupts this backoff, so neither can start a late request.
			timer := time.NewTimer(time.Duration(number-1) * time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return a
			case <-timer.C:
			}
			if ctx.Err() != nil {
				return a
			}
			sessionID = newID()
			if onRetry != nil {
				if err := onRetry(number, sessionID); err != nil {
					a.ErrorCode, a.Error = "retry_progress_failed", err.Error()
					return a
				}
			}
		}
		started := time.Now()
		a = s.askOnce(ctx, selection, prompt, sessionID)
		a.SessionID = sessionID
		attempts = append(attempts, RequestAttempt{SessionID: sessionID, ResponseID: a.ResponseID, ReturnedModel: a.ReturnedModel, Usage: a.Usage, DurationMS: time.Since(started).Milliseconds(), Error: a.Error, ErrorCode: a.ErrorCode})
		a.Attempts = attempts
		if ctx.Err() != nil || !retryEmptyResponse(a) {
			return a
		}
	}
	return a
}

func (s *Server) askOnce(ctx context.Context, selection Selection, prompt Prompt, sessionID string) (a Answer) {
	a.Prompt = prompt
	i, err := s.identity(ctx, selection.AccountID)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.responsesURL, bytes.NewReader(requestBody(selection, prompt, sessionID)))
	if err != nil {
		a.Error = "创建测试请求失败"
		return a
	}
	req.Header = identityHeaders(i)
	// This path directly uses the resolved account identity. It never passes
	// through the host's account-level session convergence or account scheduler.
	for _, key := range []string{"Session-Id", "Session_id", "Conversation_id", "X-Codex-Turn-Metadata", "X-Codex-Parent-Thread-Id", "X-Codex-Thread-Id"} {
		req.Header.Del(key)
	}
	req.Header.Set("Session_id", sessionID)
	req.Header.Set("Conversation_id", sessionID)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	t, err := s.pool.get(i.ProxyUrl)
	if err != nil {
		a.Error = err.Error()
		return a
	}
	resp, err := t.RoundTrip(req)
	if err != nil {
		a.ErrorCode = "transport_error"
		a.Error = networkError(ctx, err)
		return a
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		a.Error = upstreamError(resp.StatusCode, raw, i).Error()
		a.ErrorCode = "upstream_http"
		return a
	}
	a = parseResponse(resp.Body, resp.Header.Get("Content-Type"), prompt)
	if a.Error != "" {
		a.Error = redact(a.Error, i)
	}
	return a
}
func upstreamError(status int, raw []byte, i *pluginv1.ResolveOutboundIdentityResponse) error {
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Detail string `json:"detail"`
	}
	_ = json.Unmarshal(raw, &body)
	message := body.Error.Message
	if message == "" {
		message = body.Detail
	}
	if message == "" {
		message = http.StatusText(status)
	}
	return fmt.Errorf("上游 HTTP %d：%s", status, redact(message, i))
}
func redact(message string, i *pluginv1.ResolveOutboundIdentityResponse) string {
	for _, secret := range []string{i.Token, i.ProxyUrl} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	if u, err := url.Parse(i.ProxyUrl); err == nil && u.User != nil {
		if p, ok := u.User.Password(); ok && p != "" {
			message = strings.ReplaceAll(message, p, "[redacted]")
		}
	}
	r := []rune(message)
	if len(r) > 1500 {
		message = string(r[:1500]) + "…"
	}
	return message
}
