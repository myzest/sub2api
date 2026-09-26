package pluginconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Enabled  bool           `json:"enabled"`
	Timezone TimezoneConfig `json:"timezone"`
	Detect   DetectConfig   `json:"detect"`
}

type TimezoneConfig struct {
	Mode             string            `json:"mode"`
	CustomTZ         string            `json:"custom_tz"`
	DefaultTZ        string            `json:"default_tz"`
	EgressCacheHours int               `json:"egress_cache_hours"`
	Overrides        map[string]string `json:"overrides"`
}

type DetectConfig struct {
	TaskID      string   `json:"task_id"`
	CreatedAt   string   `json:"created_at"`
	AccountIDs  []int64  `json:"account_ids"`
	Models      []string `json:"models"`
	Repeats     int      `json:"repeats"`
	Concurrency int      `json:"concurrency"`
}

var legacyKeys = []string{}
var taskIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

func Default() Config {
	return Config{
		Enabled: true,
		Timezone: TimezoneConfig{
			Mode: "egress_ip", CustomTZ: "Asia/Singapore", DefaultTZ: "Asia/Singapore",
			EgressCacheHours: 24, Overrides: map[string]string{},
		},
		Detect: DetectConfig{
			AccountIDs: []int64{}, Models: []string{"gpt-6-astra", "gpt-5.6-sol"},
			Repeats: 3, Concurrency: 4,
		},
	}
}

// Parse overlays a strictly typed configuration onto fresh defaults. Explicit
// collection nulls normalize to empty collections; scalar nulls are rejected.
func Parse(raw []byte) (Config, error) {
	c := Default()
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return c, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Config{}, fmt.Errorf("configuration must be a JSON object: %w", err)
	}
	for _, k := range legacyKeys {
		delete(fields, k)
	}
	if err := rejectNullFields(fields, ""); err != nil {
		return Config{}, err
	}
	clean, err := json.Marshal(fields)
	if err != nil {
		return Config{}, err
	}
	d := json.NewDecoder(bytes.NewReader(clean))
	d.DisallowUnknownFields()
	if err := d.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	c.normalize()
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func rejectNullFields(fields map[string]json.RawMessage, prefix string) error {
	for k, v := range fields {
		path := prefix + strings.ToLower(k)
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			switch path {
			case "timezone.overrides", "detect.account_ids", "detect.models":
				continue
			default:
				return fmt.Errorf("%s must not be null", path)
			}
		}
		if path == "timezone" || path == "detect" {
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(v, &nested); err != nil {
				return fmt.Errorf("%s must be an object: %w", path, err)
			}
			if err := rejectNullFields(nested, path+"."); err != nil {
				return err
			}
		}
		if path == "detect.models" || path == "detect.account_ids" {
			var entries []json.RawMessage
			if err := json.Unmarshal(v, &entries); err != nil {
				return fmt.Errorf("%s must be an array: %w", path, err)
			}
			for _, entry := range entries {
				if bytes.Equal(bytes.TrimSpace(entry), []byte("null")) {
					return fmt.Errorf("%s must not contain null", path)
				}
			}
		}
	}
	return nil
}

func (c *Config) normalize() {
	if c.Timezone.Overrides == nil {
		c.Timezone.Overrides = map[string]string{}
	}
	ids := make([]int64, 0, len(c.Detect.AccountIDs))
	seenIDs := make(map[int64]bool, len(c.Detect.AccountIDs))
	for _, id := range c.Detect.AccountIDs {
		if !seenIDs[id] {
			ids = append(ids, id)
			seenIDs[id] = true
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	c.Detect.AccountIDs = ids
	models := make([]string, 0, len(c.Detect.Models))
	seenModels := make(map[string]bool, len(c.Detect.Models))
	for _, m := range c.Detect.Models {
		if m != "" && !seenModels[m] {
			models = append(models, m)
			seenModels[m] = true
		}
	}
	c.Detect.Models = models
}

func (c Config) validate() error {
	switch c.Timezone.Mode {
	case "custom", "random_stable", "egress_ip":
	default:
		return fmt.Errorf("timezone.mode must be custom, random_stable, or egress_ip")
	}
	if !InWhitelist(c.Timezone.CustomTZ) || !InWhitelist(c.Timezone.DefaultTZ) {
		return fmt.Errorf("timezone.custom_tz and timezone.default_tz must be whitelisted")
	}
	for key, tz := range c.Timezone.Overrides {
		id, err := strconv.ParseInt(key, 10, 64)
		if err != nil || id <= 0 || strconv.FormatInt(id, 10) != key {
			return fmt.Errorf("timezone.overrides key %q must be a canonical positive account ID", key)
		}
		if !InWhitelist(tz) {
			return fmt.Errorf("timezone.overrides[%q] must be whitelisted", key)
		}
	}
	if c.Timezone.EgressCacheHours < 1 || c.Timezone.EgressCacheHours > 720 {
		return fmt.Errorf("timezone.egress_cache_hours must be between 1 and 720")
	}
	if c.Detect.Repeats < 1 || c.Detect.Repeats > 10 {
		return fmt.Errorf("detect.repeats must be between 1 and 10")
	}
	if c.Detect.Concurrency < 1 || c.Detect.Concurrency > 16 {
		return fmt.Errorf("detect.concurrency must be between 1 and 16")
	}
	for _, id := range c.Detect.AccountIDs {
		if id <= 0 {
			return fmt.Errorf("detect.account_ids must contain positive account IDs")
		}
	}
	for _, model := range c.Detect.Models {
		if !contains(SelectableModels, model) {
			return fmt.Errorf("detect.models contains unsupported model %q", model)
		}
	}
	// Division avoids overflow even on configurations constructed in-process.
	if len(c.Detect.Models) > 0 && len(c.Detect.AccountIDs) > 200/len(c.Detect.Models) {
		return fmt.Errorf("detect account/model pairs must not exceed 200")
	}
	if c.Detect.TaskID != "" {
		if !taskIDPattern.MatchString(c.Detect.TaskID) {
			return fmt.Errorf("detect.task_id must match ^[A-Za-z0-9-]{1,64}$")
		}
		if _, err := time.Parse(time.RFC3339, c.Detect.CreatedAt); err != nil {
			return fmt.Errorf("detect.created_at must be RFC3339: %w", err)
		}
		if len(c.Detect.AccountIDs) == 0 || len(c.Detect.Models) == 0 {
			return fmt.Errorf("a detection task requires account_ids and models")
		}
	}
	return nil
}

func (c Config) Marshal() []byte {
	b, _ := json.Marshal(c)
	return b
}
