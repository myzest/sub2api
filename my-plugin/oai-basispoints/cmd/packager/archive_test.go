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
	"path/filepath"
	"testing"
)

func TestPackageSignatureFilesAndTamperDetection(t *testing.T) {
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.s2plugin")
	files := map[string][]byte{"ui/index.html": []byte("fixture UI"), "runtimes/linux-amd64/oai-basispoints": []byte("fixture binary")}
	if err := writePackage(path, files, map[string]runtimeEntry{"linux-amd64": {Path: "runtimes/linux-amd64/oai-basispoints"}}, key, "fixture-publisher"); err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	entries := map[string][]byte{}
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		entries[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
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
	signatureBytes, _ := base64.StdEncoding.DecodeString(sig.Signature)
	if !ed25519.Verify(pub, entries["manifest.json"], signatureBytes) {
		t.Fatal("signature invalid")
	}
	if ed25519.Verify(pub, append(entries["manifest.json"], ' '), signatureBytes) {
		t.Fatal("tampering accepted")
	}
	for name, expected := range m.Files {
		hash := sha256.Sum256(entries[name])
		if hex.EncodeToString(hash[:]) != expected {
			t.Fatal("file hash mismatch", name)
		}
	}
	if len(m.Files) != 2 || len(entries) != 4 || m.UI.Entrypoint != "ui/index.html" || len(m.Requires.Tested) != 0 {
		t.Fatal("invalid archive layout or unverified compatibility claim")
	}
}
