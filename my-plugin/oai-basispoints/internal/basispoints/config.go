package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
)

const (
	PluginID    = "local.sub2api.oai-basispoints"
	Version     = "0.1.17"
	Capability  = "openai.oauth.outbound_transport.v1"
	bpsURL      = "https://bps.openai.com/basispoints/api/responses"
	maxBody     = 8 << 20
	maxResponse = 32 << 20
)

type Config struct {
	RouteEnabled     bool     `json:"route_enabled"`
	AccountIDs       []int64  `json:"account_ids"`
	Models           []string `json:"models"`
	DefaultEffort    string   `json:"default_effort"`
	AllowUltra       bool     `json:"allow_ultra"`
	TimeoutSeconds   int      `json:"timeout_seconds"`
	ReplayTTLSeconds int      `json:"replay_ttl_seconds"`
	ImageTransport   string   `json:"image_transport"`
	Command          *Command `json:"command,omitempty"`
}

type Command struct {
	ID          string `json:"id"`
	Instance    string `json:"instance"`
	IssuedAt    int64  `json:"issued_at"`
	Action      string `json:"action"`
	AccountID   int64  `json:"account_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	ImageDetail string `json:"image_detail,omitempty"`
	ImageFormat string `json:"image_format,omitempty"`
}

var modelPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,119}$`)
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9-]{16,64}$`)

func defaultConfig() Config {
	return Config{AccountIDs: []int64{}, Models: []string{"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-6-astra"}, DefaultEffort: "medium", TimeoutSeconds: 600, ReplayTTLSeconds: 86400, ImageTransport: "attachment"}
}

func parseConfig(raw []byte) (Config, error) {
	c := defaultConfig()
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > 64<<10 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return c, errors.New("配置必须是 JSON 对象且不超过 64 KiB")
	}
	if _, err := decodeObject(raw); err != nil {
		return c, errors.New("配置必须为无重复字段的 JSON 对象")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return c, errors.New("配置字段或类型无效")
	}
	if d.Decode(new(any)) != io.EOF {
		return c, errors.New("配置包含多余数据")
	}
	if len(c.AccountIDs) > 1000 {
		return c, errors.New("账号白名单最多 1000 个")
	}
	slices.Sort(c.AccountIDs)
	c.AccountIDs = slices.Compact(c.AccountIDs)
	for _, id := range c.AccountIDs {
		if id <= 0 {
			return c, errors.New("账号 ID 必须为正整数")
		}
	}
	if c.RouteEnabled && len(c.AccountIDs) == 0 {
		return c, errors.New("启用 BPS 路由前请选择账号")
	}
	if len(c.Models) == 0 || len(c.Models) > 100 {
		return c, errors.New("请配置 1–100 个允许的模型名")
	}
	for _, m := range c.Models {
		if !modelPattern.MatchString(m) {
			return c, errors.New("模型名只能包含字母、数字、点、横线和下划线")
		}
	}
	slices.Sort(c.Models)
	c.Models = slices.Compact(c.Models)
	if _, err := effort(c.DefaultEffort, c.AllowUltra); err != nil {
		return c, err
	}
	if c.TimeoutSeconds < 10 || c.TimeoutSeconds > 3600 {
		return c, errors.New("请求超时必须为 10–3600 秒")
	}
	if c.ReplayTTLSeconds < 300 || c.ReplayTTLSeconds > 604800 {
		return c, errors.New("工具回放保留时间必须为 300–604800 秒")
	}
	if c.ImageTransport != "attachment" && c.ImageTransport != "passthrough" {
		return c, errors.New("image_transport 只允许 attachment 或 passthrough")
	}
	if cmd := c.Command; cmd != nil {
		if !idPattern.MatchString(cmd.ID) || !idPattern.MatchString(cmd.Instance) || cmd.IssuedAt <= 0 {
			return c, errors.New("操作标识无效，请重新打开插件页面")
		}
		if cmd.Action != "accounts" && cmd.Action != "clear_diagnostics" && cmd.Action != "probe" && cmd.Action != "probe_image" && cmd.Action != "probe_tools" && cmd.Action != "probe_image_route" {
			return c, errors.New("不支持的操作")
		}
		if cmd.Action != "accounts" && cmd.Action != "clear_diagnostics" {
			if cmd.AccountID <= 0 || !slices.Contains(c.Models, cmd.Model) {
				return c, errors.New("探测需要选择账号和允许的模型")
			}
			if _, err := effort(cmd.Effort, c.AllowUltra); err != nil {
				return c, err
			}
		}
		if cmd.Action == "probe_image" || cmd.Action == "probe_image_route" {
			if cmd.ImageFormat == "" {
				cmd.ImageFormat = "jpeg"
			}
			if cmd.ImageFormat != "jpeg" && cmd.ImageFormat != "png" {
				return c, errors.New("图片探测格式只允许 jpeg/png")
			}
			if cmd.ImageDetail == "" {
				cmd.ImageDetail = "high"
			}
			switch cmd.ImageDetail {
			case "auto", "low", "high", "original":
			default:
				return c, errors.New("图片探测 detail 只允许 auto/low/high/original")
			}
		}
	}
	return c, nil
}

func effort(v string, ultra bool) (string, error) {
	switch v {
	case "low", "medium", "high", "xhigh":
		return v, nil
	case "max":
		return "xhigh", nil
	case "ultra":
		if ultra {
			return v, nil
		}
	}
	return "", errors.New("不支持的 reasoning effort；允许 low/medium/high/xhigh，max 映射为 xhigh；ultra 需显式开启")
}

func (c Config) routes(id int64) bool { return c.RouteEnabled && slices.Contains(c.AccountIDs, id) }

type object = map[string]any

var errDuplicateJSONKey = errors.New("duplicate key")
var errJSONNesting = errors.New("JSON nesting limit")

// Reject duplicate keys as well as trailing data. Ambiguous tool envelopes must
// never acquire different meanings in the validator and the downstream client.
func decodeJSON(raw []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	v, err := decodeValue(d, 0)
	if err != nil {
		return nil, errors.New("无效或含重复字段的 JSON")
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("JSON 包含多余数据")
	}
	return v, nil
}

func decodeValue(d *json.Decoder, depth int) (any, error) {
	if depth > 100 {
		return nil, errJSONNesting
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		m := object{}
		for d.More() {
			k, err := d.Token()
			if err != nil {
				return nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, errors.New("invalid key")
			}
			if _, exists := m[key]; exists {
				return nil, errDuplicateJSONKey
			}
			m[key], err = decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
		}
		_, err = d.Token()
		return m, err
	case '[':
		a := []any{}
		for d.More() {
			v, err := decodeValue(d, depth+1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		_, err = d.Token()
		return a, err
	}
	return nil, errors.New("unexpected delimiter")
}

func decodeObject(raw []byte) (object, error) {
	v, err := decodeJSON(raw)
	if err != nil {
		return nil, err
	}
	m, ok := v.(object)
	if !ok {
		return nil, errors.New("必须提供 JSON 对象")
	}
	return m, nil
}
func str(m object, k string) string { s, _ := m[k].(string); return s }
func clone(m object) object {
	n := object{}
	for k, v := range m {
		n[k] = v
	}
	return n
}
func encoded(v any) []byte { b, _ := json.Marshal(v); return b }
