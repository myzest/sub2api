// Command packager builds and signs a Sub2API plugin archive from manifest.source.json.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const pluginID = "local.codex-inspector"
const modulePath = "local.sub2api/codex-inspector"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: go run ./tools/packager keygen|build [-key .signing/publisher.pem] [-publisher codex-inspector-local-v1]")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	keyPath := flags.String("key", ".signing/publisher.pem", "Ed25519 PKCS8 private key, mode 0600; never bundled")
	publisher := flags.String("publisher", "codex-inspector-local-v1", "trusted publisher key ID")
	targets := flags.String("targets", "linux/amd64,linux/arm64,darwin/arm64", "comma-separated OS/architecture targets")
	source := flags.String("source", "manifest.source.json", "source manifest")
	releaseVersion := flags.String("version", "", "optional release version; must equal source manifest version")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if !regexp.MustCompile("^[a-z0-9][a-z0-9._-]{0,99}$").MatchString(*publisher) {
		return errors.New("invalid publisher ID")
	}
	if args[0] != "keygen" && args[0] != "build" {
		return errors.New("unknown command: use keygen or build")
	}
	m, err := readManifest(*source)
	if err != nil {
		return err
	}
	if *releaseVersion != "" && *releaseVersion != m.Version {
		return fmt.Errorf("release version %q differs from manifest version %q", *releaseVersion, m.Version)
	}
	if args[0] == "keygen" {
		key, err := createKey(*keyPath)
		if err != nil {
			return err
		}
		if err := writePublicConfig(key, *publisher); err != nil {
			return err
		}
		fmt.Println("已生成本插件专用发布者密钥；公钥配置位于 dist/trusted-publisher.yaml。私钥不会被打包。")
		return nil
	}
	targetList, err := parseTargets(*targets)
	if err != nil {
		return err
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	runtimes := map[string]runtimeEntry{}
	for _, target := range targetList {
		parts := strings.Split(target, "/")
		binary := "codex-inspector"
		if parts[0] == "windows" {
			binary += ".exe"
		}
		relative := "runtimes/" + parts[0] + "-" + parts[1] + "/" + binary
		output := filepath.Join("build", filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		fmt.Println("Building", target, "version", m.Version)
		args := []string{"build", "-trimpath", "-ldflags=-s -w -X " + modulePath + "/internal/server.Version=" + m.Version, "-o", output, "./cmd/codex-inspector"}
		command := exec.Command("go", args...)
		command.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
		command.Stdout, command.Stderr = os.Stdout, os.Stderr
		if err := command.Run(); err != nil {
			return err
		}
		files[relative], err = os.ReadFile(output)
		if err != nil {
			return err
		}
		runtimes[parts[0]+"-"+parts[1]] = runtimeEntry{Path: relative}
	}
	if err := filepath.WalkDir("ui", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular UI file is not allowed: %s", path)
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return fmt.Errorf("hidden UI asset is not allowed: %s", path)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(path)] = content
		return nil
	}); err != nil {
		return err
	}
	for _, path := range []string{"LICENSE", "NOTICE.md"} {
		files["licenses/"+path], err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	// Fingerprint bank distribution must retain its license and provenance.
	for _, path := range []string{"internal/modeltrace/LICENSE.ModelTrace", "internal/modeltrace/provenance.json"} {
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		files["licenses/"+strings.ReplaceAll(path, "/", "-")] = content
	}
	m.Runtimes = runtimes
	if err := os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	archivePath := filepath.Join("dist", "codex-inspector-"+m.Version+".s2plugin")
	if err := writePackage(archivePath, m, files, key, *publisher); err != nil {
		return err
	}
	if err := writePublicConfig(key, *publisher); err != nil {
		return err
	}
	fmt.Println("Created", archivePath)
	return nil
}

func parseTargets(raw string) ([]string, error) {
	targets := strings.Split(raw, ",")
	seen := map[string]bool{}
	for _, target := range targets {
		parts := strings.Split(target, "/")
		if len(parts) != 2 || (parts[0] != "linux" && parts[0] != "darwin" && parts[0] != "windows") || (parts[1] != "amd64" && parts[1] != "arm64") || seen[target] {
			return nil, fmt.Errorf("invalid, unsupported or duplicate target %q", target)
		}
		seen[target] = true
	}
	return targets, nil
}

func createKey(path string) (ed25519.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("创建密钥失败（已有密钥不会覆盖）: %w", err)
	}
	writeErr := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	closeErr := f.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return key, nil
}

func loadKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("读取签名密钥失败，请先为本插件运行 keygen: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return nil, errors.New("私钥必须是普通文件且权限为 0600（拒绝符号链接）")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, errors.New("key must be Ed25519")
	}
	return key, nil
}

func writePublicConfig(key ed25519.PrivateKey, publisher string) error {
	if err := os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	public := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	content := "# Merge this entry into the EXISTING plugins.trusted_publishers map.\n# Keep allow_unsigned: false. Restart Sub2API after the first configuration.\nplugins:\n  trusted_publishers:\n    " + publisher + ": \"" + public + "\"\n"
	return os.WriteFile("dist/trusted-publisher.yaml", []byte(content), 0644)
}
