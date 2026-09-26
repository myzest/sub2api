package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommittedSDKPin(t *testing.T) {
	if err := verifyPin("../..", false); err != nil {
		t.Fatal(err)
	}
}
func fixturePin(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "internal", "pluginapi", "v1")
	if err := os.MkdirAll(directory, 0755); err != nil {
		t.Fatal(err)
	}
	pin := upstreamPin{Source: "upstream", Revision: strings.Repeat("a", 40), Files: map[string]string{}}
	for _, name := range sdkFiles {
		content := []byte("fixture " + name + "\n")
		hash := sha256.Sum256(content)
		pin.Files[name] = hex.EncodeToString(hash[:])
		if err := os.WriteFile(filepath.Join(directory, name), content, 0644); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(pin)
	if err := os.WriteFile(filepath.Join(root, "UPSTREAM.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	return root
}
func TestPinDetectsChangedBytesAndAdditionalFiles(t *testing.T) {
	root := fixturePin(t)
	if err := verifyPin(root, false); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "internal", "pluginapi", "v1", "plugin.proto")
	if err := os.WriteFile(file, []byte("changed"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPin(root, false); err == nil {
		t.Fatal("changed SDK bytes accepted")
	}
	root = fixturePin(t)
	if err := os.WriteFile(filepath.Join(root, "internal", "pluginapi", "v1", "extra.go"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	if err := verifyPin(root, false); err == nil {
		t.Fatal("extra SDK file accepted")
	}
}
