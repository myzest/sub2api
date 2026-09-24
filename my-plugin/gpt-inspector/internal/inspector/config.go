package inspector

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func parseConfig(raw []byte) (Config, error) {
	var cfg Config
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if len(raw) > 8192 {
		return cfg, errors.New("配置过大")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("配置只能包含一个 JSON 对象")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return cfg, errors.New("配置必须是对象")
	}
	c := cfg.Command
	if c == nil {
		return cfg, nil
	}
	if !uuidPattern.MatchString(c.ID) || !uuidPattern.MatchString(c.Instance) {
		return cfg, errors.New("命令标识无效，请重新打开插件")
	}
	switch c.Action {
	case "accounts", "models", "start", "stop", "read_batch", "read_result", "clear_account", "clear_all":
	default:
		return cfg, errors.New("不支持的插件命令")
	}
	if c.Action == "models" || c.Action == "read_batch" || c.Action == "read_result" || c.Action == "clear_account" {
		if c.AccountID <= 0 {
			return cfg, errors.New("请选择账号")
		}
	}
	if c.Action == "read_result" && (!uuidPattern.MatchString(c.BatchID) || c.Item < 0 || c.Item >= maxRounds*len(prompts)) {
		return cfg, errors.New("结果位置无效")
	}
	if c.Action == "start" {
		if c.AccountID <= 0 || c.Model == "" || len(c.Model) > 200 || c.Effort == "" || len(c.Effort) > 32 {
			return cfg, errors.New("必须选择账号、模型和推理强度")
		}
		if c.Rounds < 1 || c.Rounds > maxRounds {
			return cfg, errors.New("重复轮数必须在 1–20 之间")
		}
		if len(c.Prompts) == 0 || len(c.Prompts) > len(prompts) {
			return cfg, errors.New("请选择至少一道题")
		}
		seen := map[string]bool{}
		for _, id := range c.Prompts {
			if _, ok := findPrompt(id); !ok || seen[id] {
				return cfg, errors.New("题目不存在或重复")
			}
			seen[id] = true
		}
	}
	return cfg, nil
}
