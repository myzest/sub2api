package main

import (
	"archive/zip"
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type runtimeEntry struct {
	Path string `json:"path"`
}
type manifest struct {
	SchemaVersion int                     `json:"schema_version"`
	ID            string                  `json:"id"`
	Name          string                  `json:"name"`
	Version       string                  `json:"version"`
	Description   string                  `json:"description"`
	Author        string                  `json:"author"`
	Requires      requirements            `json:"requires"`
	Capabilities  []capability            `json:"capabilities"`
	Runtimes      map[string]runtimeEntry `json:"runtimes"`
	UI            uiEntry                 `json:"ui"`
	Files         map[string]string       `json:"files"`
}
type requirements struct {
	Sub2API        string   `json:"sub2api"`
	Recommended    string   `json:"recommended_sub2api_version"`
	Tested         []string `json:"tested_sub2api_versions"`
	PluginProtocol int      `json:"plugin_protocol"`
	TransportAPI   int      `json:"transport_api"`
	UIBridge       int      `json:"ui_bridge"`
}
type capability struct {
	ID          string `json:"id"`
	Platform    string `json:"platform"`
	AccountType string `json:"account_type"`
}
type uiEntry struct {
	Entrypoint string `json:"entrypoint"`
}
type signature struct {
	Algorithm string `json:"algorithm"`
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

func readManifest(source string) (manifest, error) {
	var m manifest
	raw, err := os.ReadFile(source)
	if err != nil {
		return m, fmt.Errorf("请在 codex-inspector 根目录执行打包工具: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return m, errors.New("manifest must contain exactly one JSON object")
	}
	version := regexp.MustCompile("^(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?(\\+[0-9A-Za-z-]+(\\.[0-9A-Za-z-]+)*)?$")
	if m.SchemaVersion != 1 || m.ID != pluginID || m.Name == "" || !version.MatchString(m.Version) {
		return m, errors.New("invalid schema, plugin ID, name, or semantic version in source manifest")
	}
	if m.Requires.Sub2API == "" || m.Requires.PluginProtocol != 1 || m.Requires.TransportAPI != 1 || m.Requires.UIBridge != 1 {
		return m, errors.New("invalid host requirements")
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != (capability{ID: "openai.oauth.outbound_transport.v1", Platform: "openai", AccountType: "oauth"}) {
		return m, errors.New("invalid transport capability")
	}
	if m.UI.Entrypoint != "ui/index.html" {
		return m, errors.New("invalid UI entrypoint")
	}
	if len(m.Files) != 0 {
		return m, errors.New("source manifest files must be empty; hashes are generated at build time")
	}
	if m.Requires.Tested == nil {
		m.Requires.Tested = []string{}
	}
	return m, nil
}

func safeArchivePath(name string) bool {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, ":") || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "." || part == ".." || part == "" {
			return false
		}
	}
	return true
}

func writePackage(output string, m manifest, files map[string][]byte, key ed25519.PrivateKey, publisher string) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("invalid signing key")
	}
	if len(m.Runtimes) == 0 {
		return errors.New("at least one runtime is required")
	}
	if _, ok := files[m.UI.Entrypoint]; !ok {
		return errors.New("UI entrypoint missing")
	}
	for _, runtime := range m.Runtimes {
		if !strings.HasPrefix(runtime.Path, "runtimes/") {
			return errors.New("invalid runtime path")
		}
		if _, ok := files[runtime.Path]; !ok {
			return errors.New("runtime file missing")
		}
	}
	m.Files = make(map[string]string, len(files))
	archiveFiles := make(map[string][]byte, len(files)+2)
	for name, raw := range files {
		if !safeArchivePath(name) || name == "manifest.json" || name == "signature.json" {
			return fmt.Errorf("unsafe or reserved archive path %q", name)
		}
		hash := sha256.Sum256(raw)
		m.Files[name] = hex.EncodeToString(hash[:])
		archiveFiles[name] = raw
	}
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	archiveFiles["manifest.json"] = raw
	sig := signature{Algorithm: "ed25519", KeyID: publisher, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, raw))}
	archiveFiles["signature.json"], err = json.MarshalIndent(sig, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(output), ".codex-inspector-*.s2plugin")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	writer := zip.NewWriter(file)
	names := make([]string, 0, len(archiveFiles))
	for name := range archiveFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0644)
		if strings.HasPrefix(name, "runtimes/") {
			header.SetMode(0755)
		}
		entry, writeErr := writer.CreateHeader(header)
		if writeErr == nil {
			_, writeErr = entry.Write(archiveFiles[name])
		}
		if writeErr != nil {
			_ = writer.Close()
			_ = file.Close()
			return writeErr
		}
	}
	if err := writer.Close(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Chmod(temporary, 0644); err != nil {
		return err
	}
	return os.Rename(temporary, output)
}
