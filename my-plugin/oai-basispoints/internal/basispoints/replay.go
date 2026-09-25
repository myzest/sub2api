package basispoints

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	pluginv1 "local.sub2api/oai-basispoints/internal/pluginapi"
)

var handlePattern = regexp.MustCompile(`^(?:fc_|ctc_|call_)?bp_([a-f0-9]{32})$`)
var inputTypePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)

type replayRecord struct {
	AccountID int64  `json:"account_id"`
	Scope     string `json:"scope"`
	ExpiresAt int64  `json:"expires_at"`
	Native    object `json:"native"`
	Client    object `json:"client"`
}
type replayStore struct {
	host      pluginv1.HostServiceClient
	accountID int64
	scope     string
	ttl       int
}

func digest(v any) string { sum := sha256.Sum256(encoded(v)); return hex.EncodeToString(sum[:]) }
func conversationScope(source object, headers http.Header) string {
	key := cacheKey(source)
	if key == "" {
		key = headers.Get("Session-Id")
	}
	if key == "" {
		key = headers.Get("Session_id")
	}
	var root any
	switch input := source["input"].(type) {
	case string:
		root = input
	case []any:
		for _, v := range input {
			item, ok := v.(object)
			if !ok {
				continue
			}
			if str(item, "role") != "user" {
				continue
			}
			// Text extraction is only for the local replay fingerprint. The
			// actual content, including multimodal parts, is forwarded intact.
			text, err := outputText(item["content"])
			if err == nil {
				root = text
			} else {
				root = item["content"]
			}
			break
		}
	}
	// A handle is a 128-bit capability as well. Outbound session IDs can be
	// merged by the host; they are not a substitute for original API-key scope.
	return digest([]any{str(source, "model"), key, root})
}
func (r replayStore) key(handle string) (string, error) {
	m := handlePattern.FindStringSubmatch(handle)
	if m == nil {
		return "", errors.New("工具调用没有本插件的回放句柄；请从新会话开始")
	}
	return fmt.Sprintf("a%d.%s.%s", r.accountID, r.scope, m[1]), nil
}
func (r replayStore) save(ctx context.Context, native, client object) error {
	if r.host == nil {
		return errors.New("工具回放需要 HostService v2")
	}
	key, err := r.key(str(client, "call_id"))
	if err != nil {
		return err
	}
	record := replayRecord{r.accountID, r.scope, time.Now().Unix() + int64(r.ttl), native, client}
	raw := encoded(record)
	if len(raw) > 256<<10 {
		return errors.New("工具调用超过宿主 256 KiB 回放存储限制")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = r.host.KVSet(ctx, &pluginv1.KVSetRequest{Namespace: "replay-v1", Key: key, Value: raw, TtlSeconds: int64(r.ttl)})
	if err != nil {
		return errors.New("保存原生工具调用失败，未向客户端释放工具")
	}
	return nil
}
func (r replayStore) load(ctx context.Context, handle string) (*replayRecord, error) {
	if r.host == nil {
		return nil, errors.New("工具回放需要 HostService v2")
	}
	key, err := r.key(handle)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, err := r.host.KVGet(ctx, &pluginv1.KVGetRequest{Namespace: "replay-v1", Key: key})
	if err != nil {
		return nil, errors.New("读取工具回放失败")
	}
	if !v.GetFound() {
		return nil, errors.New("工具回放不存在或已过期；请保持账号、模型和完整会话历史，必要时开启新会话")
	}
	var record replayRecord
	decoder := json.NewDecoder(bytes.NewReader(v.Value))
	decoder.UseNumber()
	if len(v.Value) > 256<<10 || decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF || record.AccountID != r.accountID || record.Scope != r.scope || record.ExpiresAt <= time.Now().Unix() || str(record.Native, "call_id") == "" || record.Client == nil {
		return nil, errors.New("工具回放无效或已过期")
	}
	wantKey, err := r.key(str(record.Client, "call_id"))
	if err != nil || wantKey != key {
		return nil, errors.New("工具回放身份不匹配")
	}
	return &record, nil
}

func sameCall(a, b object) bool {
	for _, k := range []string{"type", "name", "namespace"} {
		if str(a, k) != str(b, k) {
			return false
		}
	}
	if str(a, "type") == "custom_tool_call" {
		return str(a, "input") == str(b, "input")
	}
	x, err := decodeObject([]byte(str(a, "arguments")))
	if err != nil {
		return false
	}
	y, err := decodeObject([]byte(str(b, "arguments")))
	return err == nil && digest(x) == digest(y)
}

func (r replayStore) restore(ctx context.Context, input []any) ([]any, error) {
	output := []any{}
	loaded := map[string]*replayRecord{}
	calls, results := map[string]bool{}, map[string]bool{}
	for index, v := range input {
		item, ok := v.(object)
		if !ok {
			continue // Both references ignore non-object history entries.
		}
		item = cleanInput(item)
		if item == nil {
			continue
		}
		t := strings.ToLower(strings.TrimSpace(str(item, "type")))
		if t != "function_call" && t != "custom_tool_call" && t != "function_call_output" && t != "custom_tool_call_output" {
			output = append(output, item)
			continue
		}
		handle := str(item, "call_id")
		key, err := r.key(handle)
		if err != nil {
			return nil, inputError(index, item, err)
		}
		record := loaded[key]
		if record == nil {
			record, err = r.load(ctx, handle)
			if err != nil {
				return nil, inputError(index, item, err)
			}
			loaded[key] = record
		}
		isCall := t == "function_call" || t == "custom_tool_call"
		if isCall {
			if calls[key] || !sameCall(item, record.Client) {
				return nil, inputError(index, item, errors.New("回放工具调用重复或内容已改变"))
			}
			calls[key] = true
			output = append(output, record.Native)
		} else {
			if results[key] {
				return nil, inputError(index, item, errors.New("工具结果重复"))
			}
			results[key] = true
			if t != str(record.Client, "type")+"_output" {
				return nil, inputError(index, item, errors.New("工具结果类型与调用不匹配"))
			}
			if !calls[key] {
				calls[key] = true
				output = append(output, record.Native)
			}
			// Match _normalized_tool_output: retain structured output and
			// extension fields, changing only the native call identity/type.
			item["type"] = "function_call_output"
			item["call_id"] = record.Native["call_id"]
			delete(item, "name")
			delete(item, "namespace")
			id := str(record.Native, "call_id")
			if !strings.HasPrefix(id, "fc_") {
				id = "fc_" + id
			}
			item["id"] = id
			if text, ok := item["output"].(string); (ok && strings.TrimSpace(text) == "") || item["output"] == nil {
				item["output"] = "(tool call succeeded with no output)"
			}
			output = append(output, item)
		}
	}
	for key := range calls {
		if !results[key] {
			return nil, errors.New("缺少对应工具执行结果")
		}
	}
	return output, nil
}

// Include only bounded protocol labels, never message bodies, reference IDs,
// tool arguments or credential values in a client-visible diagnostic.
func inputError(index int, item object, err error) error {
	return fmt.Errorf("input[%d].type=%s: %w", index, inputTypeLabel(item), err)
}

func inputTypeLabel(item object) string {
	t := str(item, "type")
	if !inputTypePattern.MatchString(t) {
		t = "missing_or_invalid"
	}
	return t
}

func outputText(v any) (string, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	if parts, ok := v.([]any); ok {
		result := ""
		for index, p := range parts {
			if text, ok := p.(string); ok {
				result += text
				continue
			}
			part, ok := p.(object)
			if !ok {
				return "", fmt.Errorf("content/output[%d] 只支持文本", index)
			}
			field := "text"
			switch str(part, "type") {
			case "text", "input_text", "output_text":
			case "refusal":
				field = "refusal"
			default:
				return "", fmt.Errorf("content/output[%d].type=%s: 仅支持文本，不支持图片、文件或音频", index, inputTypeLabel(part))
			}
			text, ok := part[field].(string)
			if !ok {
				return "", fmt.Errorf("content/output[%d].%s 必须为字符串", index, field)
			}
			result += text
		}
		return result, nil
	}
	return "", errors.New("content/output 必须为字符串或文本数组")
}

// Follow translate_input_items in the two reference projects. BPS, rather
// than a local input-type whitelist, validates ordinary items and their parts.
func cleanInput(item object) object {
	switch strings.ToLower(strings.TrimSpace(str(item, "type"))) {
	case "reasoning":
		if str(item, "encrypted_content") == "" {
			return nil
		}
		return object{"type": "reasoning", "summary": []any{}, "encrypted_content": item["encrypted_content"]}
	case "item_reference":
		return nil // store:false requires the client to send full history.
	}
	next := clone(item)
	delete(next, "internal_chat_message_metadata_passthrough")
	return next
}
