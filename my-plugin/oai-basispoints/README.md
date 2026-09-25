# OpenAI Basis Points for Sub2API

独立的 Sub2API `.s2plugin` 插件，使用已有 OpenAI OAuth 账号，将选定账号的 Responses 请求转换为 BPS 协议。源码和生成文件都在此目录；不需要二改 Sub2API 主程序。

这是 **0.1.5 待实机验收版本**。本版保留按 CPA v0.1.8 实现的同源图片附件上传、工具适配及探测，修正附件错误状态/响应头、并发取消、工具回放数值精度和配置页状态同步问题。BPS 属于参考项目使用的非公开兼容接口，源码对齐不等于实机验收，也不证明模型“不降智”。逐项来源和边界见 [FIELD_MAPPING.md](FIELD_MAPPING.md)，审查修复见 [VALIDATION.md](VALIDATION.md)。

## 安装

适用宿主：本工作区对应 fork 的插件机制，Plugin Protocol / Transport API / UI Bridge v1，**HostService v2**。清单声明 `>=0.2.8 <0.3.0`，版本号本身不能替代这些接口要求；没有在你的服务器镜像上验收。

1. 将 `dist/trusted-publisher.yaml` 中公钥条目合并到服务器已有配置的 `plugins.trusted_publishers`，保留其他发布者，保持 `allow_unsigned: false`。首次添加公钥后重启 Sub2API。
2. 在插件管理中导入 `dist/oai-basispoints-0.1.5.s2plugin`。包内包含 Linux amd64、Linux arm64、macOS arm64 运行文件。
3. 启用本插件。如果已有 OpenAI OAuth 出站插件处于启用状态，先停用它：宿主的 `openai.oauth.outbound_transport.v1` 只有一个启用槽位，不能与 GPT Inspector 同时占用。
4. 将宿主此插件能力的灰度比例设为 **100%**，再通过本插件的账号白名单控制 BPS 路由。比例低于 100% 时，部分选定账号可能根本到不了插件。
5. 打开插件设置，刷新账号。在保持 BPS 路由关闭的情况下，选一个账号、模型和 effort，点击“保存并探测文本”。图片、工具探测也在这里；每项总共最多等待 120 秒，只有点击探测才发上游请求。
6. 探测成功后，选中需要走 BPS 的账号，再打开路由开关并保存。

默认 `route_enabled=false`、账号白名单为空。插件已启用但 BPS 路由关闭时，OAuth 请求经插件按原地址转发。

0.1.1 在后端、配置页和示例配置的默认允许模型中加入 `gpt-6-astra`。升级时先停用旧版，再导入新版并启用；沿用原发布者公钥。已保存的模型列表会保留，如需增加该模型，在「允许的模型」追加一行 `gpt-6-astra` 并保存即可，旧版也支持手动添加。

0.1.3 的字段兼容对接已由用户反馈完成。0.1.4 延续普通输入与 SSE 透传，处理后续反馈的拖图 502 和只会文字回答问题。尚未收到这两个问题的真实请求/上游错误，因此不能把推测写成已确认根因；新增探测和诊断用于你自行验收。

建议先使用**只含一个 BPS 账号的专用分组**。已有会话应重新开始，后续工具回放需要保持同一账号、模型、会话标识和完整历史。宿主其他账号故障转移策略仍由 Sub2API 管理；混用 BPS / 普通账号的分组不适合一致性验收。

关闭本插件的 BPS 路由开关可恢复普通转发；停用插件可恢复宿主原生传输。切换通道后请开启新会话。

## 账号适配的边界

已按 sub2api 格式导入的 **OpenAI OAuth 账号**可直接复用，不要求先有 Excel 对话。本插件不再次导入账号文件、不保存 OAuth token、不接管 refresh token：

- 正常转发使用宿主已准备好的 `Authorization`、ChatGPT account ID 和账号代理。
- 单账号探测通过 `ResolveOutboundIdentity` 获取宿主当前有效的短期 token。
- 缺少 account ID 请求头时，尝试从该 token 的 JWT auth claims 读取 ChatGPT account ID。sub2api 的数字账号 ID 不是 ChatGPT account ID。
- 没有凭据交换或权限生成过程。BPS 若返回 401/403，只能据此判定本次身份或访问条件不满足，不能仅通过改写 JSON 解决；也不能据此断定登录 Excel 必然能解决。

这不支持将 API Key 账号、密码或 refresh token 单独转换成 Excel 会话。

## 工作原理与支持范围

目标固定为 `https://bps.openai.com/basispoints/api/responses`。插件设置 ChatGPT 认证与 Excel 客户端 headers，发送 `model_selection: explicit`、`store: false` 和顶层 `reasoning_effort`。

| 项目 | 行为 |
| --- | --- |
| 模型 | 沿用宿主传入的模型名，要求在插件允许列表中，不自动替换。默认允许 `gpt-5.6-sol/terra/luna` 和 `gpt-6-astra`；是否可用由账号实测决定 |
| 推理强度 | `low/medium/high/xhigh` 原样传递；`max → xhigh`；`ultra` 需明确开启后原样发送，不回退到 medium |
| 输入 | 普通项保留 type、role、phase、content 和扩展字段；只删除客户端专用 `internal_chat_message_metadata_passthrough`，reasoning、引用和工具回放另行处理 |
| 流式 | 沿用客户端 stream；普通 SSE 事件、内容和扩展字段转发，工具完成校验后生成客户端工具事件。由于工具事件被重建，sequence_number 重新连续编号 |
| 工具 | 支持客户端 function/custom、namespace、allowed_tools；完整 schema / custom format 放入提示，function 参数用 JSON Schema 校验；遵守 parallel_tool_calls:false |
| 原生信封 | 仅接受 `run_officejs` / `functions.run_officejs` 的严格 JSON `code`，解析 name/arguments/input 或 tool/args；不执行 JS、不从任意文本猜测工具、不自动修补 JSON |
| 工具回放 | 完整原生 item 存入宿主 KV，客户端收到随机句柄，下一轮恢复原生 item 与原始 call ID 的 output |
| 终态 | 保留 failed/incomplete、usage、incomplete_details 与扩展字段；缺少终态、非法/重复工具、违反并行限制或原生身份不一致均报错，不伪装 completed |
| 其他账号 | 按原 URL、Host、headers、body、proxy 转发，使用标准 Go HTTP/TLS；不会复现宿主定制 TLS 指纹 |
| Token / 错误 | Token 只在内存用于请求；HTTP 错误保留状态码及限流 headers，不透传原始错误体 |

每个 `run_officejs` 信封只承载一个客户端工具；允许并行时可接收多个信封，先全部校验并保存原生回放记录，再释放客户端事件。客户端显式禁止并行时，多个调用报错。`custom.format` 会完整提供给模型，但插件不实现自定义 grammar 的本地解析器。工具仍由客户端按原来的权限与确认机制执行。

默认 `image_transport=attachment`：将 user message 中 `input_image.image_url` 的 data URL 解码成原图字节，通过同一账号和代理向同源 `/basispoints/api/attachments` 上传 multipart `file`，读取 `openai_file_id` 后改成 `file_id`。保留 detail，缺省补 auto；不压缩图片、不自动降为 low。`passthrough` 可用于对照旧行为。普通 URL、已有 file_id、文件和工具结果中的图片仍透传，不猜测跨通道文件或工具结果图片的上传规则。

附件缓存按账号、凭据、端点、MIME 和图片摘要隔离，最多 512 条，仅存摘要与文件 ID。仅复用已成功上传的结果；同时首次上传同图的请求各自上传，避免一个请求取消使其他请求失败。重启清空；上游附件寿命尚未验证，不自动重试或静默删除图片。附件非 2xx 响应即使正文读取失败，也保留 HTTP 状态和限流/追踪响应头。顶层文本和压缩项仍保留原类型与位置。

工具目录只提取 function/custom 和 namespace 下的对应叶子，其他工具声明与参考项目一样忽略；这不意味着原生搜索、计算机或 MCP 工具已经可用。不增加独立 `/responses/compact`、`/input_tokens`、后台任务或旧响应拉取能力。`previous_response_id`、`conversation`、`background:true` 明确报错。

顶层请求使用参考仓库的 BPS 字段集合：缓存键原值传递，标量 metadata 保留并字符串化，已有 task/turn/iteration 不覆盖。按 CPA v0.1.8，`context_management` 缺省/null/空数组省略，其他显式值透传，`service_tier` 透传。`parallel_tool_calls` 用于本地工具校验和提示，不直接发送。`max_output_tokens`、`temperature`、`top_p`、`text`、`truncation`、`include`、嵌套 `reasoning.summary` 及其他未映射顶层字段仍不发送；不猜测扩大上游 schema，也不在失败后删除字段重试。

由于插件收到的是宿主已规范化的出站请求，无法恢复被宿主改写的原始模型别名或原始 API Key ID。因此首版用账号白名单选择通道，不用 `-excel` / `-basispoints` 模型后缀识别。

## 回放与资源限制

工具句柄含 128 位随机值；KV key 绑定 sub2api account ID、模型、出站会话标识与首条用户消息指纹。缓存缺失、过期、参数被修改或账号变化时明确失败，不伪造原生调用。

宿主开启 `codex_fingerprint_mode=session/full` 时可能合并出站会话 ID。随机句柄能防止简单猜测，但现有插件接口**不能证明原始 API Key 级别的严格隔离**。不要将该机制描述为完整租户授权边界。

默认工具回放保留 24 小时，可设置 5 分钟至 7 天。KV 会保存原生工具参数及客户端调用，可能包含业务内容；不保存 OAuth token 或工具执行结果。插件自身也校验过期时间，实际持久化能力取决于宿主 KV / Redis 配置。

单请求最多 8 MiB，响应最多 32 MiB，单条 SSE 最多 4 MiB，单条回放记录最多 256 KiB；超限报错。配置超时默认 600 秒，客户端取消会传递至上游。插件不会自动重试已发送的 BPS 请求。

## 你可以这样验收

1. 选原有账号和模型，依次点击文本、图片、工具探测。文本/图片各一次模型请求，附件模式图片另含上传；工具最多两次模型请求，均消耗账号额度。
2. 图片探测生成六位随机数字 PNG。答案不放入提示词、文件名或图片元数据，只在像素和本地比对状态中。页面显示图片、预期/实际回复、detail、附件与 Responses 各自 HTTP、阶段及脱敏错误。默认 high，可手动比较 auto/low/original；不自动回退。回答不符不等于完全不支持图片。
3. 工具探测要求调用虚拟的 `diagnostics.read_probe`，经同一工具转换与 KV 记录后，模拟返回随机校验值，第二轮禁用工具并要求读回此值。它不访问服务器或你的桌面文件，也不证明真实客户端执行成功。
4. 从 Codex 新任务发送“列出当前项目文件，并读取其中一个文件”，观察客户端真实工具记录；随后在插件中刷新“最近路由请求”。工具类型/可调用数可区分客户端未传工具、tool_choice 禁用或工具格式不受支持；有目录但零调用时，结合终态/错误判断模型行为或转换问题。
5. 拖入图片，确认实际桌面请求成功；再验收命令执行、文件修改、工具回放、Skill 文件读取、取消、代理及关闭路由后的普通转发。

“最近路由请求”只保留内存中最后一条已结束的 BPS 请求，显示账号、模型、阶段、HTTP、工具类型/有界名称和计数，不保存 prompt、参数、图片、token；下一轮会覆盖上一轮。刷新本身不发上游请求。探测结果另行保存，包含插件生成的图片和模拟校验值。

状态查询串行处理，短暂失败后每 5 秒重试只读查询，不重发探测。刷新账号先读取服务器当前配置，保留本窗口未保存的表单；检测到配置变化会提示。宿主没有条件保存接口，跨窗口同时保存仍无法原子协调，请避免同时修改多个设置窗口。

## Codex 功能恢复边界

协议桥接能传递客户端已声明的工具及其结果，不能给未声明工具的请求添加本机访问能力。提示中已明确：存在文件/命令工具时应调用它们检查项目，按客户端指引读取 SKILL.md，不凭目录名推测文件内容。

| 能力 | 本版实现与依赖 |
| --- | --- |
| 读文件、运行命令、编辑文件 | function/custom/namespace 转换与回放；由 Codex 客户端提供工具、执行并处理权限 |
| Skills | 保留 instructions 和工具目录，提示通过工具读取 SKILL.md；发现、安装和执行环境仍归客户端 |
| Subagent | 若客户端将其声明为上述 callable 类型，可按同样信封桥接；是否启用由客户端配置及当前用户授权决定 |
| MCP / Apps | 以 function/custom 暴露时可进入目录；原生 mcp、tool_search 等服务端工具尚未映射 |
| 图片 | 新增用户消息 data URL 附件上传；普通 URL/file_id 及工具输出图片仍由上游决定是否接受 |
| 原生搜索、计算机、独立 compact/input_tokens、服务端会话 | 尚未接入，不能宣称完整恢复 |

同账号/模型/题目/effort 的多轮质量比较仍应单独进行。HTTP 200、返回模型名、一次识图或工具探测通过，都不等于全部 Codex 能力或模型质量通过验收。

401/403 重点排查 token / account ID / 访问条件；400/422 排查模型、effort 与输入兼容；429 是限流或额度信号。真实部署遇到错误时保留状态码、Response ID、插件错误码即可，无需提供 token。

## 本地构建

构建需要 Go 1.26+。UI 是静态资源；服务端运行已打包二进制不需要 Go、Node.js、Python、Excel 或浏览器。

```sh
cd /Users/zest/myworks/sub2api/my-plugin/oai-basispoints
go run ./cmd/packager keygen
go run ./cmd/packager build
```

`keygen` 只需执行一次，已有私钥不会覆盖。升级版本应复用本目录 `.signing/publisher.pem`；私钥不会打包，不能上传服务器。构建结果在 `dist/`，对应二进制在 `build/runtimes/`。

测试源码已保留，可自行选择运行：

```sh
go test -race ./...
go vet ./...
BASISPOINTS_TEST_BINARY="$PWD/build/runtimes/darwin-arm64/oai-basispoints" go test ./internal/basispoints -run TestRealPluginProcessAndHostBroker
npm ci
npm test
npx playwright install chromium
npm run test:browser
npm run verify-package
```

实际交付状态见 `VALIDATION.md`。字段依据见 `FIELD_MAPPING.md`，参考源码与许可见 `THIRD_PARTY_NOTICES.md`。
