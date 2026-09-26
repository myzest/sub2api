# Codex Inspector

面向 **Sub2API 宿主**的独立 Go 插件，插件 ID 为 local.codex-inspector，当前版本 0.1.0。它不是 Codex marketplace 插件；不需要创建 .codex-plugin/plugin.json。

提供两项功能：

- **Codex 时区与日期改写**：对 OpenAI OAuth 出站请求中受支持的 user/input_text 环境块，改写首个 timezone/current_date。可选自定义、按账号稳定随机、账号代理出口 IP 时区，以及账号覆盖与回退时区。
- **账号模型指纹检测**：用 ModelTrace 内置库，对选择的账号、请求模型与重复轮次进行统计匹配。不同账号并发，同一账号内模型与轮次串行。检测会消耗真实上游额度，必须在界面单独确认。

统计匹配**不是智商、模型能力或服务身份事实**。界面中的概率是候选间相对统计分数，不是“降智概率”。输出分布、模型更新、采样参数和指纹库覆盖都可能影响结论；不匹配不足以证明降级。

## 宿主与安装

- 宿主声明范围：Sub2API >=0.2.8 <0.3.0；需要 HostService API 2、Plugin Protocol 1、Transport API 1、UI Bridge 1。
- **单路由限制**：openai.oauth.outbound_transport.v1 只能由一个插件接管。启用前停用其他相同能力插件，包括 gpt-inspector、oai-basispoints、codex-state-keeper 或官方 OpenAI transport；按宿主管理界面的冲突提示操作。
- 本插件使用 HTTP 上游 transport。宿主在原生 WebSocket 路径需要桥接为 HTTP；它不会提供原生上游 WebSocket，也不能保证保留原生 WebSocket 的性能、会话复用等优化。
- manifest.source.json 的 tested_sub2api_versions 保持空数组。单元测试、真实插件子进程/broker 夹具和浏览器桥测试通过，不等于已在某个生产宿主版本完成端到端验证。部署前仍应在隔离宿主验证实际账号、代理、转发和 UI。

签名构建后，将 dist/trusted-publisher.yaml 中**公钥条目**合并到宿主现有 plugins.trusted_publishers；保留其他发布者配置及 allow_unsigned: false。根据宿主流程重启、上传 .s2plugin、启用并打开配置页。私钥不能放入宿主、容器镜像或仓库。

## 使用步骤

1. 在“时区设置”选择策略、回退时区及账号覆盖，点击“保存配置”。enabled 仅控制出站时区改写；关闭后继续转发，手动检测仍独立可用。
2. 在“模型检测”勾选可调度账号与模型，设置 1–10 次重复、1–16 个并发账号；账号×模型组合最多 200。
3. 核对“账号×模型×轮次”的请求数，点击“开始检测”，再确认真实额度消耗。界面先保存、核对任务 ID，然后显式执行 config.test。
4. 查看运行状态、按组合结果与已脱敏诊断。关闭弹窗不会取消已受理任务。没有手动取消接口；进程终止或租约丢失会导致中断，不会自动续跑。

账号列表仅展示 id/name/schedulable，不展示账号 metadata、token、代理 URL、密码或 cookie。UI 只通过宿主 postMessage 桥，禁止直连管理 API、读取父窗口 DOM 或发起外部请求。状态轮询不会覆盖未保存表单。

## 对参考文档的必要适配

实现参考 2026-09-24-codex-inspector.public.md；其未随附的设计文档不是已验证事实。按当前仓库运行契约作了以下调整：

- **ApplyConfig 不启动检测**。实际宿主会在数据库持久化前、临时验证进程、启动和回滚路径调用 ApplyConfig。把检测放在 ApplyConfig 会重放或吞掉任务。现在普通保存无网络探测副作用，只有显式 config.test 才受理任务。
- 任务 ID 为 d<当前16位实例ID>-<时间戳>-<随机hex>，最多 64 字符；服务端校验实例前缀、创建时间 ±30 分钟与幂等标记。旧实例保存的任务不会在重启后重新执行。
- config.test 只测试**已保存**配置。UI 执行 save → load 核对 task_id → test，再验证返回 task_id。v1 桥没有事务或 expected_task_id 参数，多个窗口仍可能在核对后竞争保存；因此不要同时在多个配置窗口启动任务。返回冲突时先检查实际任务，不要立即重试。
- accepted 表示任务已受理，不保证已取得租约。Health.dispatch 的 not_started/interrupted 会明确呈现；旧的 latest 结果不会被显示为本次成功。
- Go 版本与此仓库 SDK 的 go.mod 对齐（当前 go 1.26.0），没有为文档中的版本数字盲目升级依赖。SDK 五文件逐字节 pin 到 UPSTREAM.json，未格式化或改写。

## 已知边界

- 改写仅处理 user 消息中以 environment_context 开头的 input_text 里的首个 timezone/current_date。任意自然语言、系统消息、非文本内容、未识别结构与没有目标字段的请求不受影响。JSON 解码后处理，因此兼容宿主把尖括号编码为 Unicode 转义的情况。
- 原始 body 无需改写时保持原字节；解析/缓存/GeoIP 等失败采用保守回退。修改 body 后会重算 Content-Length；JSON 未声明字段予以保留。
- 出口 IP 查询有意走账号代理，会依赖第三方 GeoIP 可用性与准确度；失败按配置回退。检测同样走该账号代理。
- KV 没有 SET-NX，租约只是 best-effort。多副本竞争、网络延迟或崩溃可能造成重复或中断，**不保证恰好一次**。不要把它用于必须原子扣费/精确一次的调度。
- 任务和结果保留 7 天，executed 标记保留 24 小时；诊断环限流合并并脱敏。未完成任务不会自动恢复，重发会再次消耗额度。
- 只支持内置白名单时区和内置可选模型；库内没有覆盖或没有有效轮次时不能得到可靠结论。

## 本地验证

所有命令在 my-plugin/codex-inspector 目录下执行，不需要真实账号凭证：

    go run ./tools/sync-pluginapi -upstream
    test -z "$(gofmt -l cmd internal tools)"
    go vet ./...
    mkdir -p build
    go build -o build/codex-inspector ./cmd/codex-inspector
    CODEX_INSPECTOR_TEST_BINARY="$PWD/build/codex-inspector" go test ./... -count=1
    npm ci --ignore-scripts --no-audit --no-fund
    npm run check
    npx playwright install chromium
    npm test

macOS 的浏览器测试默认使用 /Applications/Google Chrome.app 内的 Chrome；也可用 CHROME_BIN 指定浏览器可执行文件。Linux CI 使用 Playwright Chromium。浏览器夹具是真实 sandbox="allow-scripts" iframe、宿主 CSP 与消息桥；测试不访问真实模型或账户。截图保存在 test-results/。

CI 位于仓库根 .github/workflows/codex-inspector-ci.yml，只影响该模块，先构建实际插件进程，再使用 CODEX_INSPECTOR_TEST_BINARY 跑 go test -race，避免集成用例被静默跳过。原有 workflow 不改动。

## 构建签名包

本次本地交付已经生成三个平台的签名包、公钥配置与校验文件，见 dist/ 和 VALIDATION.md。已有本插件专用 .signing/publisher.pem，后续修改只运行 build；不要重复 keygen 或覆盖密钥。下面的 keygen 命令仅用于全新克隆首次建立发布者。

先为**本插件**显式创建新密钥。keygen 使用 O_EXCL，拒绝覆盖已有密钥；build 仅接受权限精确为 0600 的普通 PKCS8 Ed25519 私钥文件，拒绝符号链接。不要复用或读取其他插件私钥。

    go run ./tools/packager keygen -key .signing/publisher.pem -publisher codex-inspector-local-v1
    go run ./tools/packager build -key .signing/publisher.pem -publisher codex-inspector-local-v1
    node tests/verify-package.cjs

默认平台为 linux/amd64、linux/arm64、darwin/arm64。可用 -targets linux/amd64 缩小构建目标。版本来自 manifest.source.json，编译时通过 ldflags 写入 internal/server.Version；-version 0.1.0 可额外验证发布版本必须一致。

输出：

- dist/codex-inspector-0.1.0.s2plugin：ZIP 包，包含已签名 manifest.json、精确 SHA-256 文件清单、运行时、UI 与第三方 notices。
- dist/trusted-publisher.yaml：可公开的宿主公钥配置片段。
- 验证脚本输出签名、哈希、平台与源码/构建产物一致性结果，并生成 .sha256 摘要。

仓库提供 .github/workflows/codex-inspector-package.yml 的手动签名构建。管理员可配置专用 CODEX_INSPECTOR_SIGNING_KEY（PEM 内容）与 CODEX_INSPECTOR_PUBLISHER_ID 变量；工作流只上传 Actions artifact，不自动提交、打 tag、推送或创建公开 Release。没有配置私钥时会明确失败，不退回未签名包。

若另行发布，应先更新 manifest.source.json 与运行版本、验证兼容性，再人工使用 inspector-vX.Y.Z 标签。不得凭宣称版本填入 tested_sub2api_versions。

## 来源与许可

SDK 来源 pin 见 UPSTREAM.json；ModelTrace revision、bank 与 JS 原件哈希见 internal/modeltrace/provenance.json，MIT 许可见 internal/modeltrace/LICENSE.ModelTrace。插件和复用宿主代码保留 LICENSE 与 NOTICE.md。私钥、构建缓存、node_modules 与测试截图不属于源码交付。
