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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"local.sub2api/gpt-inspector/internal/inspector"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: go run ./cmd/packager keygen|build [-key .signing/publisher.pem] [-publisher gpt-inspector-local-v1]")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ContinueOnError)
	keyPath := fs.String("key", ".signing/publisher.pem", "Ed25519 private key (never included in package)")
	publisher := fs.String("publisher", "gpt-inspector-local-v1", "trusted publisher key ID")
	targets := fs.String("targets", "linux/amd64,linux/arm64,darwin/arm64", "comma-separated OS/architecture targets")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if !regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`).MatchString(*publisher) {
		return errors.New("invalid publisher ID")
	}
	if _, err := os.Stat("ui/index.html"); err != nil {
		return errors.New("请在 gpt-inspector 根目录执行打包工具")
	}
	if os.Args[1] == "keygen" {
		if err := os.MkdirAll(filepath.Dir(*keyPath), 0700); err != nil {
			return err
		}
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(*keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fmt.Errorf("创建密钥（已有密钥不会覆盖）: %w", err)
		}
		err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err := writePublicConfig(key, *publisher); err != nil {
			return err
		}
		fmt.Println("已生成发布者密钥；公钥配置位于 dist/trusted-publisher.yaml。私钥仅保存在指定本地路径。")
		return nil
	}
	if os.Args[1] != "build" {
		return errors.New("unknown command")
	}
	key, err := loadKey(*keyPath)
	if err != nil {
		return err
	}
	files := map[string][]byte{}
	runtimes := map[string]runtimeEntry{}
	for _, target := range strings.Split(*targets, ",") {
		parts := strings.Split(target, "/")
		if len(parts) != 2 {
			return fmt.Errorf("invalid target %q", target)
		}
		if (parts[0] != "linux" && parts[0] != "darwin" && parts[0] != "windows") || (parts[1] != "amd64" && parts[1] != "arm64") {
			return fmt.Errorf("unsupported target %q", target)
		}
		name := "gpt-inspector"
		if parts[0] == "windows" {
			name += ".exe"
		}
		relative := "runtimes/" + parts[0] + "-" + parts[1] + "/" + name
		output := filepath.Join("build", filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(output), 0755); err != nil {
			return err
		}
		fmt.Println("Building", target)
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-o", output, "./cmd/gpt-inspector")
		cmd.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0", "GOOS="+parts[0], "GOARCH="+parts[1])
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		files[relative], err = os.ReadFile(output)
		if err != nil {
			return err
		}
		runtimes[parts[0]+"-"+parts[1]] = runtimeEntry{Path: relative}
	}
	if err := filepath.WalkDir("ui", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return errors.New("UI symlinks are not allowed")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(path)] = raw
		return nil
	}); err != nil {
		return err
	}
	files["licenses/Sub2API.LICENSE"], err = os.ReadFile("LICENSE")
	if err != nil {
		return err
	}
	if err := os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	path := filepath.Join("dist", "gpt-inspector-"+inspector.Version+".s2plugin")
	if err := writePackage(path, files, runtimes, key, *publisher); err != nil {
		return err
	}
	if err := writePublicConfig(key, *publisher); err != nil {
		return err
	}
	fmt.Println("Created", path)
	return nil
}
func loadKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取签名密钥失败，请先运行 keygen：%w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("私钥权限必须为 0600")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("invalid PEM private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ed, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("key must be Ed25519")
	}
	return ed, nil
}
func writePublicConfig(key ed25519.PrivateKey, publisher string) error {
	if err := os.MkdirAll("dist", 0755); err != nil {
		return err
	}
	public := base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey))
	content := "# Merge this entry into the EXISTING plugins.trusted_publishers map.\n# Keep allow_unsigned: false. Restart Sub2API after the first configuration.\nplugins:\n  trusted_publishers:\n    " + publisher + ": \"" + public + "\"\n"
	return os.WriteFile("dist/trusted-publisher.yaml", []byte(content), 0644)
}
