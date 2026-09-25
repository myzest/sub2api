package basispoints

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

type requestPlan struct {
	body   object
	stream bool
	tools  *toolCatalog
	store  replayStore
}

func (s *Server) prepare(ctx context.Context, raw []byte, accountID int64, headers http.Header, cfg Config) (*requestPlan, error) {
	source, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	model := str(source, "model")
	if !slices.Contains(cfg.Models, model) {
		return nil, errors.New("模型不在 BPS 允许列表中；不会自动替换模型")
	}
	if str(source, "previous_response_id") != "" || source["conversation"] != nil {
		return nil, errors.New("BPS 使用 store:false，请发送完整 input 历史；不支持 previous_response_id/conversation")
	}
	if v, exists := source["background"]; exists && v != false && v != nil {
		return nil, errors.New("不支持 background 响应")
	}
	stream := false
	if v, exists := source["stream"]; exists {
		var ok bool
		stream, ok = v.(bool)
		if !ok {
			return nil, errors.New("stream 必须为布尔值")
		}
	}
	requested := cfg.DefaultEffort
	if v, exists := source["reasoning_effort"]; exists {
		var ok bool
		requested, ok = v.(string)
		if !ok {
			return nil, errors.New("reasoning_effort 必须为字符串")
		}
	}
	if v, exists := source["reasoning"]; exists && v != nil {
		r, ok := v.(object)
		if !ok {
			return nil, errors.New("reasoning 必须为对象")
		}
		if e, exists := r["effort"]; exists {
			value, ok := e.(string)
			if !ok {
				return nil, errors.New("reasoning.effort 必须为字符串")
			}
			if flat, exists := source["reasoning_effort"]; exists && flat != value {
				return nil, errors.New("两处 reasoning effort 设置冲突")
			}
			requested = value
		}
	}
	e, err := effort(requested, cfg.AllowUltra)
	if err != nil {
		return nil, err
	}
	tools, err := readTools(source)
	if err != nil {
		return nil, err
	}
	store := replayStore{s.hostClient(), accountID, conversationScope(source, headers), cfg.ReplayTTLSeconds}
	if len(tools.tools) > 0 && store.host == nil {
		return nil, errors.New("工具调用需要宿主 HostService v2")
	}
	var input []any
	switch v := source["input"].(type) {
	case string:
		input = []any{message("user", v)}
	case []any:
		input = v
	case nil:
		input = []any{}
	default:
		return nil, errors.New("input 必须为字符串或数组")
	}
	items, err := store.restore(ctx, input)
	if err != nil {
		return nil, err
	}
	prologue := []any{}
	if instructions, exists := source["instructions"]; exists && instructions != nil {
		text, ok := instructions.(string)
		if !ok {
			return nil, errors.New("instructions 必须为字符串")
		}
		if text != "" {
			prologue = append(prologue, message("developer", text))
		}
	}
	prologue = append(prologue, message("developer", tools.instructions()))
	// These are the BPS wire fields used by both reference implementations.
	// Ordinary input fields are transparent; arbitrary Responses top-level
	// controls are not evidence of BPS support and are not added speculatively.
	body := object{"model": model, "model_selection": "explicit", "store": false, "stream": stream, "input": append(prologue, items...), "reasoning_effort": e}
	if key := cacheKey(source); key != "" {
		body["prompt_cache_key"] = key
	}
	// CPA v0.1.8 omits an absent/null/empty compaction policy. It forwards
	// other explicit values for upstream validation, without inventing a default.
	if policy := source["context_management"]; policy != nil {
		if entries, array := policy.([]any); !array || len(entries) > 0 {
			body["context_management"] = policy
		}
	}
	if tier, exists := source["service_tier"]; exists {
		body["service_tier"] = tier
	}
	body["metadata"] = requestMetadata(source, items)
	if len(encoded(body)) > maxBody {
		return nil, errors.New("转换后的 BPS 请求超过 8 MiB")
	}
	return &requestPlan{body: body, stream: stream, tools: tools, store: store}, nil
}
func message(role, text string) object {
	return object{"type": "message", "role": role, "content": []any{object{"type": "input_text", "text": text}}}
}
func cacheKey(source object) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
		if value := strings.TrimSpace(str(source, key)); value != "" {
			return value
		}
	}
	if metadata, ok := source["client_metadata"].(object); ok {
		for _, key := range []string{"session_id", "sessionId"} {
			if value := strings.TrimSpace(str(metadata, key)); value != "" {
				return value
			}
		}
	}
	return ""
}

func requestMetadata(source object, items []any) object {
	metadata := object{}
	if raw, ok := source["metadata"].(object); ok {
		keys := make([]string, 0, len(raw))
		for key := range raw {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			switch value := raw[key].(type) {
			case string, json.Number, bool:
				metadata[limitCharacters(key, 64)] = limitCharacters(fmt.Sprint(value), 512)
			}
		}
	}
	conversation := cacheKey(source)
	if conversation == "" {
		conversation = "anonymous"
		if len(items) > 0 {
			conversation = digest(items[0])
		}
	}
	turn, iteration := turnState(source["input"])
	defaults := object{
		"task_id":         uuidV5("ghcp-proxy/gpt-excel/" + conversation),
		"turn_id":         uuidV5("ghcp-proxy/gpt-excel/" + conversation + "/turn/" + turn),
		"agent_iteration": iteration,
	}
	for key, value := range defaults {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	return metadata
}

func limitCharacters(value string, limit int) string {
	chars := []rune(value)
	return string(chars[:min(len(chars), limit)])
}

// Excel keeps the turn ID fixed across tools for one user message, advancing
// only agent_iteration. Hash the entire prefix, not just the last user text.
func turnState(raw any) (string, string) {
	if text, ok := raw.(string); ok {
		return digest(text), "1"
	}
	input, ok := raw.([]any)
	if !ok {
		return "anonymous", "1"
	}
	lastUser := -1
	for index, value := range input {
		if item, ok := value.(object); ok && strings.EqualFold(strings.TrimSpace(str(item, "role")), "user") {
			lastUser = index
		}
	}
	prefix := input[:min(1, len(input))]
	if lastUser >= 0 {
		prefix = input[:lastUser+1]
	}
	iteration := 1
	for _, value := range input[lastUser+1:] {
		if item, ok := value.(object); ok {
			if t := str(item, "type"); t == "function_call_output" || t == "custom_tool_call_output" {
				iteration++
			}
		}
	}
	return digest(prefix), strconv.Itoa(iteration)
}

// RFC 9562 UUIDv5 with the URL namespace, as used by the references. SHA-1
// here identifies a deterministic task/turn; it is not an authorization key.
func uuidV5(name string) string {
	namespace := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	hash := sha1.New()
	hash.Write(namespace)
	hash.Write([]byte(name))
	id := hash.Sum(nil)[:16]
	id[6], id[8] = (id[6]&0x0f)|0x50, (id[8]&0x3f)|0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])
}

func bpsHeaders(in http.Header) (http.Header, error) {
	auth := strings.Fields(in.Get("Authorization"))
	if len(auth) != 2 || !strings.EqualFold(auth[0], "Bearer") {
		return nil, errors.New("宿主没有提供 OAuth Bearer token")
	}
	accountID, userID := in.Get("Chatgpt-Account-Id"), in.Get("X-Openai-Account-User-Id")
	if accountID == "" {
		accountID = in.Get("X-Openai-Account-Id")
	}
	// Claims are a routing fallback, not a token verification or an entitlement
	// grant. BPS remains the authority that accepts or rejects this identity.
	parts := strings.Split(auth[1], ".")
	if len(parts) == 3 && len(parts[1]) <= 64<<10 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			if claims, err := decodeObject(raw); err == nil {
				if info, ok := claims["https://api.openai.com/auth"].(object); ok {
					if accountID == "" {
						accountID = str(info, "chatgpt_account_id")
					}
					if userID == "" {
						userID = str(info, "chatgpt_user_id")
					}
				}
			}
		}
	}
	if accountID == "" || len(accountID) > 200 || strings.ContainsAny(accountID, "\r\n") {
		return nil, errors.New("OAuth 身份缺少可用的 ChatGPT account ID；sub2api 数字账号 ID 不能代替它")
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+auth[1])
	h.Set("Chatgpt-Account-Id", accountID)
	h.Set("X-Openai-Account-Id", accountID)
	if userID != "" && len(userID) <= 200 && !strings.ContainsAny(userID, "\r\n") {
		h.Set("X-Openai-Account-User-Id", userID)
	}
	for k, v := range map[string]string{
		"Content-Type": "application/json", "Accept": "text/event-stream", "Accept-Encoding": "identity", "Origin": "https://bps.openai.com", "Referer": "https://bps.openai.com/", "User-Agent": "Mozilla/5.0",
		"X-Basispoints-Auth-Mode":                            "chatgpt",
		"X-Openai-Internal-Basispoints-Client-Agent-Profile": "excel", "X-Openai-Internal-Basispoints-Client-Editor": "excel", "X-Openai-Internal-Basispoints-Client-Host": "office", "X-Openai-Internal-Basispoints-Client-Platform": "excel", "X-Openai-Internal-Basispoints-Client-Platform-Class": "PC", "X-Openai-Internal-Basispoints-Client-Product": "basispoints-excel-plugin", "X-Openai-Internal-Basispoints-Client-Runtime": "desktop", "X-Openai-Internal-Basispoints-Office-Host": "Excel", "X-Openai-Internal-Basispoints-Office-Platform": "PC",
		"X-Stainless-Arch": "unknown", "X-Stainless-Lang": "js", "X-Stainless-Os": "Unknown", "X-Stainless-Package-Version": "6.31.0", "X-Stainless-Retry-Count": "0", "X-Stainless-Runtime": "browser:chrome",
	} {
		h.Set(k, v)
	}
	return h, nil
}
