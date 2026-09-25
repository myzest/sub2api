package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"sort"
	"strings"

	"local.sub2api/oai-basispoints/internal/basispoints"
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

func writePackage(path string, files map[string][]byte, runtimes map[string]runtimeEntry, key ed25519.PrivateKey, publisher string) error {
	m := manifest{SchemaVersion: 1, ID: basispoints.PluginID, Name: "OpenAI Basis Points", Version: basispoints.Version,
		Description: "使用已有 OAuth 账号连接 Basis Points，支持图片附件、客户端工具回放与可视化探测。", Author: "Local Sub2API Plugins",
		Requires:     requirements{Sub2API: ">=0.2.8 <0.3.0", Recommended: "0.2.8", Tested: []string{}, PluginProtocol: 1, TransportAPI: 1, UIBridge: 1},
		Capabilities: []capability{{ID: basispoints.Capability, Platform: "openai", AccountType: "oauth"}}, Runtimes: runtimes, UI: uiEntry{Entrypoint: "ui/index.html"}, Files: map[string]string{}}
	archiveFiles := map[string][]byte{}
	for name, raw := range files {
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
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	z := zip.NewWriter(f)
	var names []string
	for name := range archiveFiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0644)
		if strings.HasPrefix(name, "runtimes/") {
			h.SetMode(0755)
		}
		w, e := z.CreateHeader(h)
		if e == nil {
			_, e = w.Write(archiveFiles[name])
		}
		if e != nil {
			z.Close()
			f.Close()
			return e
		}
	}
	if err := z.Close(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
