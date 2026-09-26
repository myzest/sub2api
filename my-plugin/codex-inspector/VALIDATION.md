# 验证记录

验证日期：2026-09-26。执行环境：macOS arm64，Go 1.26.4。所有账号/代理/上游测试均为本地测试夹具，未使用真实账号。

| 项目 | 结果 |
| --- | --- |
| gofmt、go vet | 通过 |
| SDK SHA-256 pin + 当前宿主源码比对 | 五文件全部一致 |
| go test -race ./... -count=1 | 191 项（含子用例），0 失败，0 跳过；11 个测试包通过 |
| 原始 ModelTrace JS / Go golden 对拍 | 16 组真实参考回答，prediction/used_outputs 一致，分数/概率差 <=1e-9 |
| 浏览器测试 | 8/8 通过；真实 Chrome、opaque-origin iframe、宿主 CSP、离线模拟桥 |
| UI 视觉检查 | 时区页、检测页、移动布局；确认悬停文字可读，测试 XSS 显示为文本 |
| go-plugin 实际子进程 | 握手、GetInfo、HostService v2 broker、配置、状态、改写与响应转发通过 |
| 已签名包中的本机二进制 | 再次执行真实子进程集成测试通过 |
| 安装包独立 Node.js 验证 | Ed25519、公钥匹配、文件 SHA-256、源码/UI/编译产物一致性、篡改拒绝全部通过 |
| 私钥进入安装包 | 否 |

## 已生成安装包

- 路径：dist/codex-inspector-0.1.0.s2plugin
- 大小：16,556,516 bytes
- SHA-256：bf08ece0537d036cfb7af2c03c6e06e2e904edbde1169192c8ccfa69d30c2d88
- 包内文件：12
- 平台：darwin-arm64, linux-amd64, linux-arm64
- 公钥配置：dist/trusted-publisher.yaml
- SHA-256 校验文件：dist/codex-inspector-0.1.0.s2plugin.sha256

Linux amd64/arm64 已交叉编译并校验包内哈希；此 macOS 环境没有执行 Linux 二进制。未连接生产 Sub2API、真实 OAuth 账号或真实 GeoIP 服务；manifest 的 tested_sub2api_versions 因此保持空数组。新增 GitHub workflows 已做静态核对，本次未推送或执行远端 CI。

首次包核对曾发现 UI 最后一次样式修订尚未入包；已等待源文件冻结后完整重建，最终验签和源码一致性检查均通过。交付的是重建后的包。

## 复现命令

所有命令在本插件目录执行：

    go run ./tools/sync-pluginapi -upstream
    test -z "$(gofmt -l cmd internal tools)"
    go vet ./...
    go build -o build/codex-inspector ./cmd/codex-inspector
    CODEX_INSPECTOR_TEST_BINARY="$PWD/build/codex-inspector" go test -race ./... -count=1
    npm run check
    npm test
    go run ./tools/packager build -version 0.1.0
    node tests/verify-package.cjs
    CODEX_INSPECTOR_TEST_BINARY="$PWD/build/runtimes/darwin-arm64/codex-inspector" go test ./internal/server -run '^TestRealPluginProcessAndHostBroker$' -count=1

签名密钥已为本插件生成并保存在 .signing/publisher.pem（0600，Git 忽略）；后续只运行 build，不再次 keygen，也不覆盖该密钥。
