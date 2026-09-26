package pluginconfig

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"testing"
)

func TestUIDefaultsMatchGoDefault(t *testing.T) {
	src, err := os.ReadFile("../../ui/app.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile("(?s)var DEFAULTS = (\\{.*?\n  \\});").FindSubmatch(src)
	if m == nil {
		t.Fatal("DEFAULTS not found in ui/app.js")
	}
	var ui, goDef map[string]any
	if err := json.Unmarshal(m[1], &ui); err != nil {
		t.Fatalf("DEFAULTS must be strict JSON: %v", err)
	}
	if err := json.Unmarshal(Default().Marshal(), &goDef); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ui, goDef) {
		t.Fatalf("ui DEFAULTS != Go Default()\nui: %s\ngo: %s", m[1], Default().Marshal())
	}
}
