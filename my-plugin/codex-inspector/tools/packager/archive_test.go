package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixtureManifest() manifest {
	return manifest{SchemaVersion: 1, ID: pluginID, Name: "Codex Inspector", Version: "0.1.0", Requires: requirements{Sub2API: ">=0.2.8 <0.3.0", Recommended: "0.2.8", Tested: []string{}, PluginProtocol: 1, TransportAPI: 1, UIBridge: 1}, Capabilities: []capability{{ID: "openai.oauth.outbound_transport.v1", Platform: "openai", AccountType: "oauth"}}, Runtimes: map[string]runtimeEntry{"linux-amd64": {Path: "runtimes/linux-amd64/codex-inspector"}}, UI: uiEntry{Entrypoint: "ui/index.html"}, Files: map[string]string{}}
}
func TestPackageSignatureFilesAndTamperDetection(t *testing.T) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "fixture.s2plugin")
	files := map[string][]byte{"ui/index.html": []byte("fixture UI"), "runtimes/linux-amd64/codex-inspector": []byte("fixture binary")}
	if err := writePackage(output, fixtureManifest(), files, key, "fixture-publisher"); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(output)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	entries := map[string][]byte{}
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		entries[entry.Name], err = io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(entry.Name, "runtimes/") && entry.Mode().Perm() != 0755 {
			t.Fatal("runtime is not executable")
		}
	}
	var sig signature
	var m manifest
	if err := json.Unmarshal(entries["signature.json"], &sig); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(entries["manifest.json"], &m); err != nil {
		t.Fatal(err)
	}
	signatureBytes, err := base64.StdEncoding.DecodeString(sig.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if sig.Algorithm != "ed25519" || sig.KeyID != "fixture-publisher" || !ed25519.Verify(public, entries["manifest.json"], signatureBytes) {
		t.Fatal("signature invalid")
	}
	if ed25519.Verify(public, append(entries["manifest.json"], ' '), signatureBytes) {
		t.Fatal("manifest tampering accepted")
	}
	for name, expected := range m.Files {
		hash := sha256.Sum256(entries[name])
		if hex.EncodeToString(hash[:]) != expected {
			t.Fatal("file hash mismatch", name)
		}
	}
	if len(m.Files) != 2 || len(entries) != 4 || m.ID != pluginID || len(m.Requires.Tested) != 0 {
		t.Fatal("invalid layout or unverified compatibility claim")
	}
	for name := range entries {
		if strings.Contains(name, ".pem") || strings.Contains(name, ".signing") {
			t.Fatal("private key leaked")
		}
	}
}
func TestPackageRejectsUnsafeAndMissingFiles(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	for _, name := range []string{"../secret", "/absolute", "ui/../secret", "ui\\secret", "manifest.json", "signature.json", "ui//secret", "C:/secret"} {
		t.Run(name, func(t *testing.T) {
			files := map[string][]byte{"ui/index.html": {}, "runtimes/linux-amd64/codex-inspector": {}, name: {}}
			if err := writePackage(filepath.Join(t.TempDir(), "bad.s2plugin"), fixtureManifest(), files, key, "fixture"); err == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
	if err := writePackage(filepath.Join(t.TempDir(), "bad.s2plugin"), fixtureManifest(), map[string][]byte{"ui/index.html": {}}, key, "fixture"); err == nil {
		t.Fatal("missing runtime accepted")
	}
}
func TestCreateKeyDoesNotOverwriteAndLoadRequires0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private.pem")
	original, err := createKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createKey(path); err == nil {
		t.Fatal("existing key was overwritten")
	}
	loaded, err := loadKey(path)
	if err != nil || !original.Equal(loaded) {
		t.Fatal("key does not round-trip", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadKey(path); err == nil {
		t.Fatal("insecure permissions accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "key-link.pem")
	if err := os.Symlink(path, link); err == nil {
		if _, err := loadKey(link); err == nil {
			t.Fatal("key symlink accepted")
		}
	}
}
func TestReadManifestAndTargetValidation(t *testing.T) {
	m, err := readManifest("../../manifest.source.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != pluginID || m.Version != "0.1.0" || len(m.Requires.Tested) != 0 {
		t.Fatal("unexpected source manifest")
	}
	for _, value := range []string{"", "linux/amd64,linux/amd64", "plan9/amd64", "linux/386", "../../linux/amd64"} {
		if _, err := parseTargets(value); err == nil {
			t.Fatalf("target accepted: %q", value)
		}
	}
	if targets, err := parseTargets("linux/amd64,linux/arm64,darwin/arm64"); err != nil || len(targets) != 3 {
		t.Fatal("default targets failed", err)
	}
	m.Files["ui/index.html"] = "incorrect"
	raw, _ := json.Marshal(m)
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readManifest(path); err == nil {
		t.Fatal("pre-filled hashes accepted")
	}
}
