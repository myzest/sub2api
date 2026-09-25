# OpenAI Basis Points for Sub2API

独立的 Sub2API `.s2plugin` 插件，使用已有 OpenAI OAuth 账号，将选定账号的 Responses 请求转换为 BPS 协议。源码和生成文件都在此目录；不需要二改 Sub2API 主程序。

这是 **0.1.10 工具链路兼容与诊断版本**。增加完整 JSON 围栏、最多两层重复原生信封的兼容；原生回放 KV 未命中时，允许根据客户端提供的完整调用及结果转换历史。新增解析失败类别、错误来源和回放范围指纹，工具探测升级为三轮。保留 0.1.9 修复。真实桌面任务仍由用户验收；本轮未运行功能测试或真实账号探测。源码依据见 [FIELD_MAPPING.md](FIELD_MAPPING.md)，交付记录见 [VALIDATION.md](VALIDATION.md)。

## 安装

适用宿主：本工作区对应 fork 的插件机制，Plugin Protocol / Transport API / UI Bridge v1，**HostService v2**。清单声明 `>=0.2.8 <0.3.0`，版本号本身不能替代这些接口要求；没有在你的服务器镜像上验收。

1. 将 `dist/trusted-publisher.yaml` 中公钥条目合并到服务器已有配置的 `plugins.trusted_publishers`，保留其他发布者，保持 `allow_unsigned: false`。首次添加公钥后重启 Sub2API。
2. 在插件管理中导入 `dist/oai-basispoints-0.1.10.s2plugin`。包内包含 Linux amd64、Linux arm64、macOS arm64 运行文件。
3. 启用本插件。如果已有 OpenAI OAuth 出站插件处于启用状态，先停用它：宿主的 `openai.oauth.outbound_transport.v1` 只有一个启用槽位，不能与 GPT Inspector 同时占用。
4. 将宿主此插件能力的灰度比例设为 **100%**，再通过本插件的账号白名单控制 BPS 路由。比例低于 100% 时，部分选定账号可能根本到不了插件。
5. 打开插件设置，刷新账号。在保持 BPS 路由关闭的情况下，选一个账号、模型和 effort，点击“保存并探测文本”。图片、工具探测也在这里；每项总共最多等待 120 秒，只有点击探测才发上游请求。
6. 探测成功后，选中需要走 BPS 的账号，再打开路由开关并保存。

默认 `route_enabled=false`、账号白名单为空。插件已启用但 BPS 路由关闭时，OAuth 请求经插件按原地址转发。

0.1.1 在后端、配置页和示例配置的默认允许模型中加入 `gpt-6-astra`。升级时先停用旧版，再导入新版并启用；沿用原发布者公钥。已保存的模型列表会保留，如需增加该模型，在「允许的模型」追加一行 `gpt-6-astra` 并保存即可，旧版也支持手动添加。

0.1.3 的字段兼容对接已由用户反馈完成。0.1.4 延续普通输入与 SSE 透传，处理后续反馈的拖图 502 和只会文字回答问题。当时尚未收到这两个问题的真实请求/上游错误，新增探测和诊断供用户验收。

用户已反馈 0.1.5 的三个探测按钮通过，但桌面端仍不能读取源码或使用 Subagent。旧工具探测直接声明顶层工具，没有覆盖宿主搬迁工具目录的路径；旧诊断也只统计顶层，不能据其“无工具”判定客户端未提供工具。0.1.6 同时读取两处声明，诊断显示工具来源，并将工具探测改为 additional_tools 中的 namespaced custom 工具。

建议先使用**只含一个 BPS 账号的专用分组**。初次验收从新任务开始。0.1.8 可转换完整的外部工具调用及对应结果，因此普通通道的旧任务不再仅因非插件 call_id 被拒；但已有 BPS 句柄仍绑定同一账号、模型、会话标识与完整历史。宿主其他账号故障转移策略仍由 Sub2API 管理；混用 BPS / 普通账号的分组不适合一致性验收。

关闭本插件的 BPS 路由开关可恢复普通转发；停用插件可恢复宿主原生传输。外部历史转换不保证跨模型的加密 reasoning、跨通道 file_id 可用，也不支持把 BPS 句柄反向迁回普通通道；遇到这类状态问题仍需新任务。

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
| 工具 | 从顶层 tools 与 input[].additional_tools 提取 function/custom、namespace；支持 allowed_tools；完整 schema / custom format 放入提示，function 参数用 JSON Schema 校验；遵守 parallel_tool_calls:false |
| 原生信封 | 接受 `run_officejs` / `functions.run_officejs` 的单 JSON 对象，可去除完整单一 JSON/无语言围栏、解开最多两层 name/arguments 原生信封；保留重复键、尾随内容、工具目录和 Schema 校验；不修非法转义、不执行 JS |
| 工具回放 | KV 命中时校验并恢复原生 item；KV 未命中且有完整调用/结果时，按参考 fallback 转成历史输入；不跨范围读 KV、不执行旧调用 |
| 终态 | 保留 failed/incomplete、usage、incomplete_details 与扩展字段；缺少终态、非法/重复工具、违反并行限制或原生身份不一致均报错，不伪装 completed |
| 其他账号 | 按原 URL、Host、headers、body、proxy 转发，使用标准 Go HTTP/TLS；不会复现宿主定制 TLS 指纹 |
| Token / 错误 | Token 只在内存用于请求；HTTP 错误保留状态码及限流 headers，并附诊断 ID；后台采集受限的脱敏校验信息，不透传原始错误体 |

每个 `run_officejs` 信封只承载一个客户端工具；允许并行时可接收多个信封，先全部校验并保存原生回放记录，再释放客户端事件。客户端显式禁止并行时，多个调用报错。`custom.format` 会完整提供给模型，但插件不实现自定义 grammar 的本地解析器。工具仍由客户端按原来的权限与确认机制执行。

默认 `image_transport=attachment`：将 user message 中 `input_image.image_url` 的 data URL 解码成原图字节，通过同一账号和代理向同源 `/basispoints/api/attachments` 上传 multipart `file`，读取 `openai_file_id` 后改成 `file_id`。保留 detail，缺省补 auto；不压缩图片、不自动降为 low。`passthrough` 可用于对照旧行为。普通 URL、已有 file_id、文件和工具结果中的图片仍透传，不猜测跨通道文件或工具结果图片的上传规则。

0.1.8 明确映射 `image/jpeg → image.jpg`、`image/png → image.png`、`image/gif → image.gif`、`image/webp → image.webp`；兼容 `image/jpg` 并将其 MIME 规范为 `image/jpeg`。不再采用 `mime.ExtensionsByType` 的首项，避免宿主 MIME 数据库选出 `.jfif` / `.jpe`。附件模式中的其他 MIME 在上传前明确报错，不伪装为受支持格式。Responses 图片项仍只改为 file_id 并补缺省 detail，不添加未经参考验证的 MIME/filename 字段。

附件缓存按账号、凭据、端点、上传格式版本、MIME、扩展名和图片摘要隔离，最多 512 条，仅存摘要与文件 ID。仅复用已成功上传的结果；同时首次上传同图的请求各自上传，避免一个请求取消使其他请求失败。缓存只在当前进程，升级启动新实例就会清空，不存在跨进程持久化的附件 ID 缓存。上游附件寿命尚未验证，不自动重试或静默删除图片。附件非 2xx 响应即使正文读取失败，也保留 HTTP 状态和限流/追踪响应头。顶层文本和压缩项仍保留原类型与位置。

工具目录从顶层和 additional_tools 提取 function/custom 和 namespace 下的对应叶子，其他工具声明与参考项目一样忽略。additional_tools 在原位置转成 developer 工具目录提示，不作为 BPS 原生工具声明发送；相同叶子定义去重，冲突报错。保留 custom exec 的 description、format 和调用时的原始 input。此适配不代表原生搜索、计算机、MCP 或 tool_search 已可用；独立 namespace group tool_choice 尚不支持。不增加独立 `/responses/compact`、`/input_tokens`、后台任务或旧响应拉取能力。`previous_response_id`、`conversation`、`background:true` 明确报错。

顶层请求使用参考仓库的 BPS 字段集合：缓存键原值传递，标量 metadata 保留并字符串化，已有 task/turn/iteration 不覆盖。按 CPA v0.1.8，`context_management` 缺省/null/空数组省略，其他显式值透传，`service_tier` 透传。`parallel_tool_calls` 用于本地工具校验和提示，不直接发送。`max_output_tokens`、`temperature`、`top_p`、`text`、`truncation`、`include`、嵌套 `reasoning.summary` 及其他未映射顶层字段仍不发送；不猜测扩大上游 schema，也不在失败后删除字段重试。

由于插件收到的是宿主已规范化的出站请求，无法恢复被宿主改写的原始模型别名或原始 API Key ID。因此首版用账号白名单选择通道，不用 `-excel` / `-basispoints` 模型后缀识别。

## 回放与资源限制

工具句柄含 128 位随机值；KV key 绑定 sub2api account ID、模型、出站会话标识与首条用户消息指纹。原生记录仍只在同范围内读取；命中时核对调用内容、身份和过期时间，不跨账号或会话搜索记录。

0.1.10 修正对 KV 未命中的无条件拒绝。即使用户没有主动切换任务，宿主自动选账号、会话键或首条上下文变化也会导致未命中。此时仅依据客户端本次提供的完整调用及配对结果构造历史 run_officejs 输入，与参考项目的 fallback 对应；不恢复原生 opaque 状态、不写新回放记录、不重新执行命令。结果必须含 output 字段；孤立结果、缺失结果、重复调用/结果、非法参数仍拒绝。已找到但损坏、身份不符、内容被修改或明确过期的记录，以及存储读取错误，不按未命中处理。跨模型加密推理状态和文件 ID 的可移植性不在此修复范围。

外部历史指不含本插件 `bp_` 标记的 function/custom 调用：参考 CPA `fallbackTransportCall` 与 Excel `_fallback_transport_call`，按完整工具名（包括 namespace）、原始字符串 input 或 JSON 对象 arguments 构造历史 run_officejs 信封，并配对原 call_id 的结果。它只重建本次请求已提供的历史，不读取其他账号 KV、不保存新执行状态，也不会把旧调用发送给客户端执行。工具已退出本轮目录或 tool_choice=none 不妨碍历史重建；这些限制仍约束新输出调用。缺失 ID/名称/参数、结果早于调用、重复或缺少结果均明确失败，不猜测参数、不丢弃历史。原始 BPS 服务端私有状态无法由普通客户端历史还原，跨通道续接并非无条件保证。

0.1.9 的回放比较区分合法空字符串 input 与缺失/null/非字符串 input，拒绝会被误当作无 namespace 的对象、数字等错误类型。重复叠加 `fc_` / `ctc_` / `call_` 后仍带 `bp_` 标记的 ID 归为异常插件句柄并拒绝，不会降级为外部历史；这不扩大有效 KV 句柄的格式。

宿主开启 `codex_fingerprint_mode=session/full` 时可能合并出站会话 ID。随机句柄能防止简单猜测，但现有插件接口**不能证明原始 API Key 级别的严格隔离**。不要将该机制描述为完整租户授权边界。

默认工具回放保留 24 小时，可设置 5 分钟至 7 天。KV 会保存原生工具参数及客户端调用，可能包含业务内容；不保存 OAuth token 或工具执行结果。插件自身也校验过期时间，实际持久化能力取决于宿主 KV / Redis 配置。

单请求最多 8 MiB，响应最多 32 MiB，单条 SSE 最多 4 MiB，单条回放记录最多 256 KiB；超限报错。配置超时默认 600 秒，客户端取消会传递至上游。插件不会自动重试已发送的 BPS 请求。

## 你可以这样验收

1. 选原有账号和模型，依次点击文本、图片、工具探测。文本/图片各一次模型请求，附件模式图片另含上传；工具最多三次模型请求，均消耗账号额度。工具探测先逐字核对 custom 多行输入，再核对 function 嵌套参数和大整数，最后用 tool_choice=none 核对两次模拟结果；不执行这些样本文本。
2. 图片探测生成六位随机数字图。默认 JPEG，可手动选择 PNG 对照；每次点击只发所选格式的一次模型请求。答案不放入提示词、文件名或图片元数据，只在像素和本地比对状态中。页面显示格式、图片、预期/实际回复、detail、附件与 Responses 各自 HTTP、阶段及脱敏错误。默认 high，可手动比较 auto/low/original；不自动回退。回答不符不等于完全不支持图片。
3. 工具探测在 `input[].additional_tools` 声明虚拟的 `diagnostics.read_probe`（custom）及 `diagnostics.echo_probe`（function），通过同一转换与 KV 链路完成两次调用，第三轮禁用工具并核对两次模拟结果。它不访问服务器或你的桌面文件，也不证明真实客户端执行成功。
4. 从 Codex 新任务发送“列出当前项目文件，并读取其中一个文件”，观察客户端真实工具记录；随后在插件中刷新“最近路由请求”。检查声明来源是否有 `input.additional_tools`、目录是否有 `exec` / 文件工具或 `collaboration.spawn_agent`（以客户端实际名称为准）。诊断另显示转换前的 BPS 原生工具名称；有目录但零调用时，结合终态/错误判断模型行为或转换问题。
5. 拖入图片，确认实际桌面请求成功；再验收命令执行、文件修改、工具回放、Skill 文件读取、取消、代理及关闭路由后的普通转发。

“保存并探测图片路由”：先保存当前设置，要求 BPS 路由已开启且所选账号在白名单内；沿用模型、effort、图片传输、所选 JPEG/PNG 格式和 detail，并带 `input.additional_tools`、`tool_choice=auto`、`parallel_tool_calls=false`。它经过实际 Forward 路由入口、附件、Responses 和 SSE 返回链路，结果写入图片请求历史；最多一次模型请求，附件模式另有一次上传。虚拟工具没有执行器，若模型返回调用则提示未完成识图验收。此按钮不会自动开启路由，也不是对用户拖图请求的原样重放。

“图片路由诊断”独立保留本实例最近 10 条已结束的图片请求；“最近路由请求”保留最近 20 条。普通文字请求不会挤掉图片历史。可选择旧记录，切回“最新完成请求”后刷新会跟随最新。请求来源区分真实客户端和主动图片路由探测；显示版本、实例及诊断 ID，进程重启清空历史，刷新只读状态。

图片摘要分别显示转换前后位置、来源（data URL、file_id、远程 URL 或对象等）、detail、MIME 与编码长度，最多 16 项；不保留图片内容、URL 或 file_id 值。附件诊断展示上传/复用数量与独立 HTTP；Responses 展示 HTTP、Request ID、请求大小和受限错误字段。上游错误体最多读取 64 KiB，仅提取 message/code/type/param/detail 中校验字段，去除 rejected input/ctx、已知请求值、凭据、URL 和长不透明字符串；非 JSON、过大或读取失败只记录原因，不保存完整错误体。客户端错误附本地诊断 ID，便于关联。工具诊断仍只保留类型、有界名称和数量。探测结果另外保留插件生成的图片和校验值。

定位拖图 400 时，先运行“保存并探测图片路由”，再在 Codex 新任务拖入图片，点击“刷新图片诊断”选择对应客户端记录。优先比较附件是否成功、转换后的图片来源、detail，以及上游 error.param/校验路径。不要仅凭 400 判定 original、某模型或图片能力不受支持；本版没有自动改 detail、删图或重试。

0.1.8 附件摘要另显示最多 16 张图的上游位置、插件生成的文件名、MIME、原图字节数和上传/复用状态，不记录原始文件名。旧任务诊断增加历史工具项计数与 ID 分类（插件/外部/缺失/异常），不显示真实 call_id 或参数。升级后首次发同图应显示新上传以及 `image.jpg / image/jpeg`；如果仍报 `got none`，该记录可区分文件名修正是否生效，不能仅凭本次代码修正宣称远端问题已解决。

0.1.9 增加附件上传尝试次数与逐图 HTTP 状态。汇总 HTTP、Content-Type、Request ID 和错误采集状态仅表示最近一次上传尝试：前图成功、后图取消或连接失败时不会沿用之前的 HTTP 200；有缓存命中也不会掩盖后续上传失败。仅命中缓存时显示“缓存复用，无上传请求”。附件非 2xx 与 Responses 使用同一套受限脱敏规则；缺少错误详情时显示读取失败、超限、非 JSON 等采集原因。

0.1.10 的“工具信封诊断”记录类型、字节数、JSON 错误类别/偏移、围栏处理和解包层数；偏移以当前层去除外围空白/围栏后交给解码器的内容为基准，不记录参数正文。解析成功仍要通过目录、Schema、tool_choice 和回放保存校验。“回放范围指纹”可用于对照同一任务各轮范围是否变化，账号单独显示；KV 未命中本身不能证明过期。“Responses HTTP”是上游状态，“客户端 HTTP”是已发给宿主的状态；流式 200 后仍可能发生工具转换错误。

模型白名单拒绝使用 bps_model_not_allowed，并标明 plugin_local_config、responses_started=false。gpt-6-sol 与 gpt-5.6-sol 不自动互换，也不因本次修复加入默认列表。其他错误分别标识本地请求、历史恢复、图片附件、上游 HTTP、信封解析、响应转换及取消/超时。

本次另有独立宿主源码修正：正常 Redis 缓存 miss 不再作为错误；日志区分传输响应错误、请求取消、WS 首帧阶段，并为选号失败补充上一失败状态。插件包不包含宿主二进制，这些宿主变化需要重新构建并部署 Sub2API 才会生效；插件 0.1.10 不依赖它们。未改变 WS 超时、账号调度或重试策略。

发行 ZIP 中的 `sub2api-bps-0.1.10-host.patch` 单独包含上述三处宿主文件改动，基于工作区提交 `089fe4ea1`。应用到其他 fork 前应检查差异；仅导入 `.s2plugin` 不会应用此补丁。

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
