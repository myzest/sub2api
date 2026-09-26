// Command sync-pluginapi verifies the vendored SDK against its committed byte hashes.
// It never rewrites SDK files or silently updates UPSTREAM.json.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

type upstreamPin struct {
	Source   string            `json:"source"`
	Revision string            `json:"revision"`
	Files    map[string]string `json:"files"`
}

var sdkFiles = []string{"manifest.schema.json", "plugin.pb.go", "plugin.proto", "plugin_grpc.pb.go", "runtime.go"}

func main() {
	root := flag.String("root", ".", "plugin module root")
	upstream := flag.Bool("upstream", false, "also compare the upstream source directory byte-for-byte (monorepo only)")
	flag.Parse()
	if err := verifyPin(*root, *upstream); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("pluginapi: all five pinned files match UPSTREAM.json")
}
func verifyPin(root string, compareUpstream bool) error {
	var pin upstreamPin
	raw, err := os.ReadFile(filepath.Join(root, "UPSTREAM.json"))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &pin); err != nil {
		return err
	}
	if len(pin.Files) != len(sdkFiles) || !regexp.MustCompile("^[a-f0-9]{40}$").MatchString(pin.Revision) {
		return errors.New("UPSTREAM.json requires the exact five SDK files and a 40-character revision")
	}
	found, err := os.ReadDir(filepath.Join(root, "internal", "pluginapi", "v1"))
	if err != nil {
		return err
	}
	if len(found) != len(sdkFiles) {
		return errors.New("vendored SDK directory must contain exactly the five pinned files")
	}
	for _, name := range sdkFiles {
		expected, ok := pin.Files[name]
		if !ok || !regexp.MustCompile("^[a-f0-9]{64}$").MatchString(expected) {
			return fmt.Errorf("invalid SHA-256 pin for %s", name)
		}
		paths := []string{filepath.Join(root, "internal", "pluginapi", "v1", name)}
		if compareUpstream {
			paths = append(paths, filepath.Join(root, pin.Source, name))
		}
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("SDK path is not a regular file: %s", path)
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			hash := sha256.Sum256(content)
			if hex.EncodeToString(hash[:]) != expected {
				return fmt.Errorf("SDK byte hash mismatch: %s; restore pinned bytes, do not silently update the pin", path)
			}
		}
	}
	names := make([]string, 0, len(pin.Files))
	for name := range pin.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		if name != sdkFiles[i] {
			return errors.New("unexpected SDK filename")
		}
	}
	return nil
}
