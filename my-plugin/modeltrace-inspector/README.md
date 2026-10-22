# ModelTrace Inspector

选择 Sub2API 已有 OpenAI OAuth 账号，运行独立数字挑战，在插件后端按 ModelTrace 指纹库计算候选概率并保存完整回答。当前版本 **0.1.0**，插件 ID `local.sub2api.modeltrace-inspector`。

所有源码、测试和打包工具均在本目录。独立 Go 模块，不依赖旧插件运行，不修改宿主源码。运行容器无需 Node.js、Python、浏览器或额外数据库。

## 安装

1. 首次安装，将 `dist/trusted-publisher.yaml` 中的公钥条目合并到宿主现有 `plugins.trusted_publishers`，保留其他发布者和 `allow_unsigned: false`，然后重启宿主加载信任配置。不要上传 `.signing/` 中的私钥。
2. 在「插件管理」停用当前占用 OpenAI OAuth 出站能力的插件，导入 `dist/modeltrace-inspector-0.1.0.s2plugin` 并启用。
3. 打开插件弹窗，选择账号，获取该账号的模型目录，手动选择模型和推理强度，设置轮数后开始。

当前宿主只开放 `openai.oauth.outbound_transport.v1`，并通过它授予账号读取权限，所以插件也承担普通 OAuth HTTP 出站转发。它与 GPT Inspector、Codex Inspector、oai-basispoints 等同能力插件不能同时启用。命中的原生 WebSocket 请求会按宿主规则转为 HTTP Bridge。普通 HTTP 转发不改写日期、时区、提示词或模型。

宿主范围声明为 `>=0.2.16 <0.3.0`，要求 HostService 2 / Plugin 1 / Transport 1 / UI Bridge 1。已核对工作区 0.2.16 和官方 0.2.17 的插件接口一致；尚未验证真实部署，因此 `tested_sub2api_versions` 留空，启用时遵循宿主的未测试版本确认。未来升级仍需检查接口与兼容范围。同 ID 更新按宿主流程先停用、导入新版本、再启用。

## 测试方式

- 只列出已有 OpenAI OAuth 账号；暂停调度的账号显示状态但不能发起测试。无需重新导入账号 JSON 或填写 Token。API Key 中转不在此插件范围内。
- 模型与档位来自该账号的 Codex 模型目录。默认均为空，切换账号、模型或插件进程后重新选择；`none` 是显式档位，不等于未选择。目录失败直接显示错误，不自动使用默认模型或改走其他协议。
- 每轮固定三条独立挑战，随机要求 292–332 个、范围为 1–355 的整数，使用上游 ModelTrace 的中文模板。数字由模型直接生成，工具列表为空，不提供搜索或代码执行。
- 默认一轮，允许 1–20 轮。一轮三条挑战并行，保存该轮归因后才启动下一轮。不同账号可以继续添加并行批次；同账号同时一个批次。基础请求数为 `账号数 × 轮数 × 3`。
- 每条请求独立生成会话头与 `prompt_cache_key`，`store:false`，不带历史或 `previous_response_id`。`instructions` 固定为空字符串，避免额外风格指令影响数字指纹；管理员选择的模型与 `reasoning.effort` 原样发送，不设置 temperature 或 seed。
- 使用宿主提供的临时 Token、Codex 身份头和账号代理，直接访问 `https://chatgpt.com/backend-api/codex/models` 与 `/responses`。宿主负责 Token 生命周期；插件不持久化凭据。这是账号直测，不运行真实 Codex 客户端，也不经过普通网关的分组、计费、模型映射或提示词处理。测试消耗所选账号上游额度，返回的 usage 可在请求信息查看。

## 超时、重试与后台任务

单条默认超时 30 分钟，可选 15 / 30 / 60 / 120 分钟或不限时，也可手动停止。不限时取消插件的总时长限制，上游与代理仍可能断开连接。

尚未收到正文或推理摘要时，连接错误、流中断和提前结束最多自动重试两次，间隔 1 / 2 秒。各次尝试用新的独立会话，共用原请求截止时间；一轮最多九次实际请求。收到部分内容、明确 HTTP 错误、上游失败、无效事件或大小超限时不自动重放。完整回答数字不足不自动补测，显示其诊断和实际有效样本数。错误不会被算作模型不一致。

关闭弹窗、刷新页面或关闭浏览器不停止已受理任务。停用插件、升级或重启容器会中断任务；重开后保留已经持久化的回答与已完成轮次的归因，未完成项标记中断，不自动续跑。进度读取不调用模型。

本版针对单实例、一个容器。请在同一弹窗内添加多账号任务。宿主 Bridge 的保存与执行不是事务；插件使用命令 ID、进程 ID 和过期校验避免重复执行，跨窗口并发启动仍可能产生冲突并明确报错。

## 结果解释与存储

每轮分别显示所有候选概率、第一候选、家族概率、分布相似度、有效样本数、三份完整回答与提示词、会话和响应 ID、耗时及错误。每个批次固定记录指纹库 revision、SHA-256 和构建时间。上游返回的 model 字段只是响应元数据，与指纹候选分开展示。

归因复用 ModelTrace 的 `75% nuisance-Hellinger + 25% ordered blocks` 算法。仅正常完成的回答参加计算；数字不足的样本按上游规则忽略。只有一份或两份有效回答时使用对应校准，零份有效则不显示候选。三轮测试分别产生三份归因，不把九份回答套用三查询校准。

概率是参考库内、均匀先验下的相对归因，不是智商、降智概率或经过认证的模型身份。库包含 18 个候选（8 GPT、10 Claude）；未覆盖的请求模型会明确标注，仍可查看原文和库内最近似候选，不自动映射别名。全部相同、连续递增/递减和重复回答只作质量提示，不改写原文或修改上游数学分数。

每账号保留最新批次。新批次运行时暂时保留上一批，待新结果提交成功后清理旧批次，避免保存失败导致两批都丢失。支持单账号清理、全部清空；清理会先取消对应任务，防止迟到回答重新生成数据。

批次发生存储错误时停止后续测试，并在结果页显示原因。已经完整保存的回答保留原始状态；如果后续索引提交恢复成功，这些回答仍按原质量规则参与归因，存储故障不会被伪装成无效模型回答。

结果使用宿主隔离 KV。回答 gzip 压缩分片保存，较大的批次索引也压缩，遵守宿主单值 256 KiB 限制。显示的是本插件 KV 值的字节数，不包含 Redis 索引和内存开销。只保存原文、测试参数与诊断，不保存账号 Token 或代理凭据。回答以纯文本展示，不执行模型输出代码。

## 开发与验证

需要 Go 1.26+；Node.js 和 Playwright 仅用于开发期 UI 测试。以下命令均在本目录执行：

```sh
go build -o build/modeltrace-inspector ./cmd/modeltrace-inspector
INSPECTOR_TEST_BINARY="$PWD/build/modeltrace-inspector" go test -race ./...
go vet ./...
npm ci --ignore-scripts --no-audit --no-fund
npm test
```

浏览器测试采用模拟宿主 Bridge、真实 iframe sandbox 和 CSP，不调用真实模型。算法对照覆盖 18 候选 × 1/2/3 份回答，共 54 组；这是实现一致性验证，不是独立模型识别准确率。重新生成对照数据：

```sh
node tools/modeltrace-parity/main.mjs /path/to/pinned/ModelTrace
```

首次构建生成本插件专用签名密钥，后续只运行 build，保留原发布者身份：

```sh
go run ./cmd/packager keygen
go run ./cmd/packager build
node tests/verify-package.cjs
```

默认生成 Linux amd64、Linux arm64、macOS arm64 运行时。`dist/` 中包含签名安装包、公钥片段和 SHA-256；私钥、构建产物及开发依赖不提交 Git。具体验证结果见 `VALIDATION.md`，需求边界记录见 `IMPLEMENTATION_NOTES.md`。

## 来源

ModelTrace 来源为 [xqy2006/ModelTrace](https://github.com/xqy2006/ModelTrace)，固定 revision `6cd4a8ff0f3f4e49d6d6969dd2fbadfa1569fde2`，保留 MIT 许可和来源哈希。公开插件 SDK 与复用的宿主/插件代码来源见 `NOTICE.md`、`UPSTREAM.json`，相应许可随安装包发布。
