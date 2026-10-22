# 验证记录

验证日期：2026-10-22。所有开发改动位于本插件目录，宿主和已有插件未修改。

## 后端与协议

在 Go 1.26.4 下执行：

```sh
go build -o build/modeltrace-inspector ./cmd/modeltrace-inspector
INSPECTOR_TEST_BINARY="$PWD/build/modeltrace-inspector" go test -race ./...
go vet ./...
```

全部通过。真实子进程用例实际启动本插件，通过 go-plugin / gRPC broker 初始化模拟宿主，检查账号读取、健康状态和 HTTP 转发；没有使用真实账号凭据。

后台用例覆盖三挑战并行、轮次屏障、多账号并行、独立会话、断流与有限重试、部分正文保留、停止、清理后的迟到回答、保存失败、存储恢复及结果完整性。连续两批完整 20 轮（120 条挑战）的替换与恢复遵守宿主单 KV 值 256 KiB 限制。

复审增加两项故障回归：索引暂时失败后最终提交恢复，已保存完整回答仍计入归因；存储未就绪时，命令读取错误与其他账号后台错误更新保持同步。两项均先复现原问题，再通过修复后的针对性测试和完整 `-race` 回归。

## ModelTrace 一致性

固定上游 revision：`6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2`。

- 内置库与上游文件逐字节一致，SHA-256 为 `f267d0513aa06981d024ec5394746fceddf44e4e8a75fa6bf8304255f4b32d6d`。
- 18 个候选分别以 1、2、3 份回答归因，共 54 组对照；概率、分数、相似度等与上游 JavaScript 的误差不超过 `1e-10`。
- 每轮独立使用 1/2/3 份有效样本对应的校准，不跨轮合并。
- 不完整响应不参与归因；规律序列等质量提示不改变上游评分公式。

这些对照复用参考样本，验证实现一致性，不代表独立模型识别准确率。

## 界面

使用 Node.js 24.17.0、Playwright 1.60.0 与本机 Chrome，在模拟宿主 Bridge、`sandbox="allow-scripts"` 和宿主 CSP 条件下执行 `npm test`：10 项全部通过，0 失败、0 跳过。

覆盖默认空模型/档位、固定三挑战、关闭弹窗后重开、逐轮概率与原文、移动端横向布局、多账号追加/停止/清理、目录失败与过期响应、未覆盖模型、部分/零有效样本、短批次更新及插件重启。模型输出以文本显示，测试中的 HTML 不执行，没有向外部站点发送请求。

复审覆盖旧轮次迟到错误、同条原文重新读取、当前读取错误可见，以及批次存储错误与完整原文并存。截图保存在本地忽略目录 `test-results/`。

## 签名安装包

执行 `go run ./cmd/packager build` 与 `node tests/verify-package.cjs`，三平台构建及校验通过。

- 产物：`dist/modeltrace-inspector-0.1.0.s2plugin`，16,367,631 字节。
- SHA-256：`02554a3961faaf1fa0ad12229a80036aa1f9bf179ef437b4856e70c556a838b6`。
- 运行时：Linux amd64、Linux arm64、macOS arm64。
- Ed25519 签名与安装公钥匹配；全部文件哈希、打包 UI 与本地源码、打包运行时与构建产物匹配。
- 包中保留 Sub2API / ModelTrace 许可与来源记录，不包含私钥。

## 宿主范围

工作区 revision 为 `6e067b2a398f7c445f0e1bcdb22f073f560254b9`（0.2.16）；对照官方 revision 为 `dba770d56a0ee0ae5bad1b3dc5e11614d323163c`（0.2.17）。公开 SDK 的四个文件哈希已验证，插件接口、账号身份和管理入口一致。没有合并官方分支或升级宿主。

真实容器导入、真实 OAuth 上游和账号代理尚未验证。manifest 的 `tested_sub2api_versions` 保持空数组；不能将模拟宿主测试当作真实部署验证。
