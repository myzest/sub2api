# OpenAI Basis Points for Sub2API

独立的 Sub2API `.s2plugin` 插件，使用已有 OpenAI OAuth 账号，将选定账号的 Responses 请求转换为 BPS 协议。源码和生成文件都在此目录；不需要二改 Sub2API 主程序。

这是 **0.1.19 旧会话透传与诊断边界复核版本**。撤销 0.1.17 仅因 agent_message 中出现 encrypted_content 就本地拒绝整条请求的限制，恢复原始两个参考项目的普通 input 透传规则：保留加密字段、明文片段、作者/接收者与顺序，由 BPS 校验；不猜测解密、不改标明文、不删除消息。保留已经获得用户成功反馈的 encrypted_function_args: [] 新委派修复。单账号探测增加 ID、时间和来源，避免与客户端路由记录混淆。0.1.19 进一步让图片转换和图片计数跳过不透明加密段的内部字段，并区分主动图片路由探测与真实客户端记录；这是静态审查发现的边界修复，不是已证明的历史故障根因。仅编译和归档检查，未运行功能测试、真实请求或部署。入口区别见 [HOST_INTEGRATION.md](HOST_INTEGRATION.md)。

## 安装

适用宿主：本工作区对应 fork 的插件机制，Plugin Protocol / Transport API / UI Bridge v1，**HostService v2**。清单声明 `>=0.2.8 <0.3.0`，版本号本身不能替代这些接口要求；没有在你的服务器镜像上验收。

1. 将 `dist/trusted-publisher.yaml` 中公钥条目合并到服务器已有配置的 `plugins.trusted_publishers`，保留其他发布者，保持 `allow_unsigned: false`。首次添加公钥后重启 Sub2API。
2. 在插件管理中导入 `dist/oai-basispoints-0.1.19.s2plugin`。包内包含 Linux amd64、Linux arm64、macOS arm64 运行文件。Windows 上运行 Codex 客户端不要求服务端插件也有 Windows 运行文件。
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

文本目标固定为 `https://bps.openai.com/basispoints/api/responses`。插件设置 ChatGPT 认证与 Excel 客户端 headers，发送 `model_selection: explicit`、`store: false` 和顶层 `reasoning_effort`。独立生图/改图使用同源 `/basispoints/api/images/generations` 和 `/basispoints/api/images/edits`，不发送上述文本专用字段。

| 项目 | 行为 |
| --- | --- |
| 模型 | 沿用宿主传入的模型名，要求在插件允许列表中，不自动替换。默认允许 `gpt-5.6-sol/terra/luna` 和 `gpt-6-astra`；是否可用由账号实测决定 |
| 推理强度 | API 输入 `low/medium/high/xhigh` 支持大小写及首尾空白规范化；`max/x-high/extra-high/extra_high → xhigh`；`ultra` 需明确开启；未知值报错 |
| 输入 | 普通项保留 type、role、phase、content 和扩展字段；只删除客户端专用 `internal_chat_message_metadata_passthrough`，reasoning、引用和工具回放另行处理 |
| 流式 | 沿用客户端 stream；普通 SSE 事件、内容和扩展字段转发，工具完成校验后生成客户端工具事件。由于工具事件被重建，sequence_number 重新连续编号 |
| 工具 | 从顶层 tools 与 input[].additional_tools 提取 function/custom、namespace；支持 allowed_tools；完整 schema / custom format 放入提示，function 参数用 JSON Schema 校验；遵守 parallel_tool_calls:false |
| 生图 / 改图 | 独立 Images JSON 请求；gpt-image-2、PNG、参考支持的参数。生图发 JSON，改图转 multipart；响应 JSON 交给宿主计费和 Codex 图片执行器 |
| 原生信封 | `run_officejs` / `functions.run_officejs` 的 code 接受对象、JSON 字符串、围栏/赋值/文字内的首个完整对象；字符串内部非法反斜杠按参考加倍，合法转义保留；最多两层重复包装。重复键、多对象、不完整结构、未声明工具或 Schema 不匹配仍拒绝，不执行外围 JS |
| 工具回放 | KV 命中时校验并恢复原生 item；KV 未命中且有完整调用/结果时，按参考 fallback 转成历史输入；不跨范围读 KV、不执行旧调用 |
| 终态 | 保留 failed/incomplete、usage、incomplete_details。收到真实响应 ID 且所有项均完整 done、索引可靠且末项为非 commentary 消息或工具时，断流可按参考恢复 completed，并标记诊断；不补造 usage。半项、仅 commentary/reasoning 或身份不一致仍失败 |
| 其他账号 | 按原 URL、Host、headers、body、proxy 转发，使用标准 Go HTTP/TLS；不会复现宿主定制 TLS 指纹 |
| Token / 错误 | 不主动把请求凭据写入诊断；HTTP 错误保留状态码及限流 headers，并附诊断 ID；自 0.1.16 按用户要求记录上游错误字段原文，不脱敏。只取限定字段，不复制整个请求/响应，原文可能含上游回显的输入 |

每个 `run_officejs` 信封只承载一个客户端工具；允许并行时保留所有能通过校验的调用，禁止并行时仅保留第一个有效调用。无法转换的兄弟调用不拖垮有效调用，但全部无效时仍报错；重复 ID、Schema、tool_choice 及 KV 保存约束保留。保留项全部保存后才释放工具事件，后续 SSE 索引随之调整。已声明且类型匹配的直接原生 function/custom 也可转换，原生 update_plan 适配参考参数与结果格式。`custom.format` 完整提供给模型，插件不执行 custom grammar；实际执行由客户端权限控制。

默认 `image_transport=attachment`：user/message 的 `input_image.image_url` data URL 仍直接上传同源 `/basispoints/api/attachments`，使用同账号和代理，保留字节、MIME、detail，缺省 detail 补 auto。工具结果等其他 input 类型中的普通内嵌图片递归处理，先 inline；不遍历 type=encrypted_content 的不透明段，正常同级图片仍按原规则处理；Responses 明确返回 400/422 后，按类型改用附件并在进程内记住。普通 URL、已有 file_id 和文件继续透传；不取回跨通道文件。`passthrough` 完全关闭这套上传和图片回退，保留原样对照。

附件明确映射 `image/jpeg → image.jpg`、`image/png → image.png`、`image/gif → image.gif`、`image/webp → image.webp`，兼容 `image/jpg`。不使用可能选出 `.jfif` / `.jpe` 的 MIME 数据库首项，不把不支持的格式伪装为 PNG。Responses 图片项只改 file_id 并补缺省 detail，不添加未经参考验证的 MIME/filename 字段。

附件缓存仍按账号、凭据、端点、格式和图片摘要隔离，最多 512 条，进程重启清空。400/422 优先清除本次使用的旧缓存并重传；本请求新上传的图片不因再次拒绝重复上传。之后逐类将 inline 改附件，仍被拒则用明确 `[image content omitted: ...]` 占位。无法解码/上传也采用占位；一轮上传网络不可用后跳过后续新上传，但仍可用成功缓存。重试受请求期限和单调次数边界限制，保留原始 turn/task/iteration；401/403/429/5xx 不触发图片回退。诊断显示省略数量，省略任何图片时不能当作识图通过；主动图片路由探测会判定失败。

流式收到 created/in_progress 后，静默 15 秒发送一次 in_progress 保活，不新增上游请求，取消时关闭读取器。最后一个完整 SSE block 即使缺少空行也会处理；已收到终态后的尾部断线不推翻结果。非流式只对可识别的 HTTP EOF/帧中断重试一次，JSON/信封/Schema 错误、普通超时不自动重试；已经开始的流式响应不重发。独立 Images 生图/改图不采用这些 Responses 重试规则。

工具目录从顶层和 additional_tools 提取 function/custom 和 namespace 下的对应叶子，其他工具声明与参考项目一样忽略。additional_tools 在原位置转成 developer 工具目录提示，不作为 BPS 原生工具声明发送；相同叶子定义去重，冲突报错。保留 custom exec 的 description、format 和调用时的原始 input。此适配不代表原生 image_generation、搜索、计算机、MCP 或 tool_search 已可用；独立 namespace group tool_choice 尚不支持。不增加独立 `/responses/compact`、`/input_tokens`、后台任务或旧响应拉取能力。`previous_response_id`、`conversation`、`background:true` 明确报错。

顶层请求使用参考仓库的 BPS 字段集合：缓存键原值传递，标量 metadata 保留并字符串化，已有 task/turn/iteration 不覆盖。按 CPA v0.1.8，`context_management` 缺省/null/空数组省略，其他显式值透传，`service_tier` 透传。`parallel_tool_calls` 用于本地工具校验和提示，不直接发送。`max_output_tokens`、`temperature`、`top_p`、`text`、`truncation`、`include`、嵌套 `reasoning.summary` 及其他未映射顶层字段仍不发送；不猜测扩大上游 schema，也不在失败后删除字段重试。

由于插件收到的是宿主已规范化的出站请求，无法恢复被宿主改写的原始模型别名或原始 API Key ID。因此首版用账号白名单选择通道，不用 `-excel` / `-basispoints` 模型后缀识别。

## 子智能体与旧会话兼容（0.1.18）

用户已反馈 0.1.17 新子智能体调用可用，但旧会话继续时在 prepare 阶段收到本地 HTTP 400 / bps_agent_encrypted_content；Responses 尚未发送。确认的原因是插件把“存在加密字段”直接当作“不支持/旧消息”，并在恢复工具历史之前拒绝整条请求。25 条子消息中即使只有一个加密段也会触发，不是 BPS 本次实际解密失败的证据。

- 0.1.18 移除该本地拒绝，采用 excel-codex-bridge 8a277df 的 translate_input_items 及 CPA 708082d 的普通 input 保留规则。agent_message 包括 encrypted_content 原样进入请求；不解密、改类型、转占位文字或丢弃旧消息，reasoning/compaction 处理不变。
- 保留 0.1.17 对新 function 工具的 encrypted_function_args: [] 明文标记与 direct 原生元数据处理。恢复旧历史与避免新错误标记是两个不同层次，不互相替代。
- 升级后可以直接在原会话重试，无须先清空诊断、删除历史或新建任务。诊断应显示“原始加密字段及消息顺序已保留”；仍需看 Responses 是否发送和最终结果。
- 透传不是解密或修复密文。如果 BPS 真正返回 invalid_encrypted_content，保留其原文、Request ID 和错误来源，不重试删字段、不换模型/账号，也不伪装成功。该情况下加密来源及账号/状态兼容性需要继续定位，不能承诺任意跨通道密文可用。

“保存并探测文本”单独构造固定短文本，不读取 Codex 会话。按钮结果在“单账号探测”区，包含独立探测 ID 和时间；“最近路由请求”同时收录客户端请求和主动图片路由探测：按来源区分，图片路由探测按结果中的 route_diagnostic_id 关联；其他记录的失败不能代替文本探测结论。两类故障都需要处理，但证据不能混用。

## 上游错误与子智能体 502（0.1.16）

- 子智能体的独立模型请求经过与父任务相同的 Responses 路由；工具目录含 `collaboration.spawn_agent` 或启动已接受，不代表其模型请求已完成。插件没有另设 Subagent 禁用分支。
- 用户提供的两次账号 #58 / gpt-6-astra 请求均为 BPS HTTP 200 后流内失败，未输出工具。旧记录未保存原始事件，无法补回当时的 `code/type/message/param`，也不能据此认定是额度、模型白名单、并发限制或会话长度。
- 新版将显式 `error` / `response.error` 归为“BPS 流内错误事件”，保留结束状态与上游错误事件类型，正常结束插件传输；不把失败恢复为 completed，不重试这些显式错误，不执行或保存暂存工具。非流式无完整响应对象时返回 JSON error / 客户端 HTTP 502，仍记录真实上游 HTTP 200。真正断网、无可靠终态或取消仍是失败。
- 上游 HTTP/SSE/JSON 错误只采集 `message/code/type/param/detail/error` 中支持的字段，字符串不替换、不脱敏，整体上限 64 KiB；detail 最多 8 项、loc 最多 16 段。缺字段、非 JSON、超限或读取失败显示采集状态，不伪造原因。不主动添加 Authorization、Cookie、完整对话或工具参数。错误原文本身可能回显这些内容，分享记录前需自行检查。
- 升级后确认当前版本和新实例号；清空旧诊断，在目标设备仅复现一次父任务或子任务，刷新对应的新记录，查看“上游错误事件”“上游错误采集”“上游诊断（错误字段）”。宿主既有错误包装/换号可能仍表现为 502，按诊断 ID 和 Request ID 关联，不调整账号、模型或权限来猜测修复。

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
2. 图片探测生成六位随机数字图。默认 JPEG，可手动选择 PNG 对照；每次点击只发所选格式的一次模型请求。答案不放入提示词、文件名或图片元数据，只在像素和本地比对状态中。页面显示格式、图片、预期/实际回复、detail、附件与 Responses 各自 HTTP、阶段及上游错误字段。默认 high，可手动比较 auto/low/original；不自动回退。回答不符不等于完全不支持图片。
3. 工具探测在 `input[].additional_tools` 声明虚拟的 `diagnostics.read_probe`（custom）及 `diagnostics.echo_probe`（function），通过同一转换与 KV 链路完成两次调用，第三轮禁用工具并核对两次模拟结果。它不访问服务器或你的桌面文件，也不证明真实客户端执行成功。
4. 从 Codex 新任务发送“列出当前项目文件，并读取其中一个文件”，观察客户端真实工具记录；随后在插件中刷新“最近路由请求”。检查声明来源是否有 `input.additional_tools`、目录是否有 `exec` / 文件工具或 `collaboration.spawn_agent`（以客户端实际名称为准）。诊断另显示转换前的 BPS 原生工具名称；有目录但零调用时，结合终态/错误判断模型行为或转换问题。
5. 拖入图片，确认实际桌面请求成功；再验收命令执行、文件修改、工具回放、Skill 文件读取、取消、代理及关闭路由后的普通转发。

“保存并探测图片路由”：先保存当前设置，要求 BPS 路由已开启且所选账号在白名单内；沿用模型、effort、图片传输、所选 JPEG/PNG 格式和 detail，并带 `input.additional_tools`、`tool_choice=auto`、`parallel_tool_calls=false`。它经过实际 Forward 路由入口、附件、Responses 和 SSE 返回链路，结果写入图片请求历史；最多一次模型请求，附件模式另有一次上传。虚拟工具没有执行器，若模型返回调用则提示未完成识图验收。此按钮不会自动开启路由，也不是对用户拖图请求的原样重放。

“图片路由诊断”独立保留本实例最近 10 条已结束的图片请求，包括 0.1.12 新增的生图和改图；“最近路由请求”保留最近 20 条。普通文字请求不会挤掉图片历史。可选择旧记录，切回“最新完成请求”后刷新会跟随最新。请求来源区分真实客户端和主动图片路由探测；显示版本、实例及诊断 ID，进程重启清空历史，刷新只读状态。生图/改图单独显示 Images HTTP、操作类型与输出图片条目数，不保存 prompt、输入图片或生成图片内容。

0.1.11 的“清空全部诊断历史”同时清空本实例所有设备的这两类摘要，显示上次清空时间。清空与请求归档使用同一把锁及代次校验：清空时已进入诊断的在途请求可以正常完成，但不重新进入列表。它不删除工具回放 KV、附件缓存、会话或单账号探测结果，不取消请求，也不调用上游。操作沿用 UI Bridge v1 的管理命令及实例/时效/重复 ID 校验；发送命令前加载服务器最新配置，不提交未保存的表单。跨窗口同时保存仍受宿主缺少条件保存接口的限制。

定位某台设备的生图请求时，先让其他设备暂停新请求，清空后在目标设备重试，结束后刷新诊断。一次用户任务可能包含读取技能、调用工具、生图及最终回复等多次请求；应检查本次新增的各条记录。下拉项增加诊断 ID 前缀，详情列出未桥接类型；没有 hosted `image_generation` 声明不能排除客户端通过 function/custom 或动态执行器提供的 `image_gen.imagegen`。摘要名称最多 32 个，截断时会提示。0.1.12 的独立文生图请求即使输入图片为 0，也会进入“图片路由诊断”。

图片摘要分别显示转换前后位置、来源（data URL、file_id、远程 URL 或对象等）、detail、MIME 与编码长度，最多 16 项；不保留图片内容、URL 或 file_id 值。附件诊断展示上传/复用数量与独立 HTTP；Responses 展示 HTTP、Request ID、请求大小和受限错误字段。上游错误体最多读取 64 KiB，仅提取 message/code/type/param/detail 中校验字段，去除 rejected input/ctx、已知请求值、凭据、URL 和长不透明字符串；非 JSON、过大或读取失败只记录原因，不保存完整错误体。客户端错误附本地诊断 ID，便于关联。工具诊断只保留类型、有界名称、数量及下述受限机器字段。探测结果另外保留插件生成的图片和校验值。

定位拖图问题时，先运行“保存并探测图片路由”，再发送真实图片并刷新对应记录。诊断保留每次回退的 HTTP、Request ID 和错误字段原文，以及最终图片字段与省略数量；不能仅凭最终 HTTP 200 判定识图成功。上传/复用是跨尝试的处理次数，不是不同图片的数量。detail 不自动修改，重试不等于确认了某个字段或模型不受支持。

0.1.8 附件摘要另显示最多 16 张图的上游位置、插件生成的文件名、MIME、原图字节数和上传/复用状态，不记录原始文件名。旧任务诊断增加历史工具项计数与 ID 分类（插件/外部/缺失/异常），不显示真实 call_id 或参数。升级后首次发同图应显示新上传以及 `image.jpg / image/jpeg`；如果仍报 `got none`，该记录可区分文件名修正是否生效，不能仅凭本次代码修正宣称远端问题已解决。

0.1.9 增加附件上传尝试次数与逐图 HTTP 状态。汇总 HTTP、Content-Type、Request ID 和错误采集状态仅表示最近一次上传尝试：前图成功、后图取消或连接失败时不会沿用之前的 HTTP 200；有缓存命中也不会掩盖后续上传失败。仅命中缓存时显示“缓存复用，无上传请求”。附件非 2xx 与 Responses 使用同一套有界错误采集规则（0.1.16 起按用户要求保留原文）；缺少错误详情时显示读取失败、超限、非 JSON 等采集原因。

“工具信封诊断”记录类型、原始字节数、JSON 错误类别/偏移、对象提取、反斜杠处理数量和解包层数；偏移基于当前层提取/兼容后实际解码的文本，不是原始请求偏移，不记录参数正文。解析成功仍要通过目录、Schema、tool_choice 和回放保存校验。“回放范围指纹”用于对照各轮范围，KV 未命中本身不能证明过期。Responses HTTP 与客户端 HTTP 分别表示上游和已发送给宿主的状态；新增重试记录、保活、恢复终态及跳过工具计数。

模型白名单拒绝使用 bps_model_not_allowed，并标明 plugin_local_config、responses_started=false。gpt-6-sol 与 gpt-5.6-sol 不自动互换，也不因本次修复加入默认列表。其他错误分别标识本地请求、历史恢复、图片附件、上游 HTTP、信封解析、响应转换及取消/超时。

0.1.10 另有独立宿主源码修正：正常 Redis 缓存 miss 不再作为错误；日志区分传输响应错误、请求取消、WS 首帧阶段，并为选号失败补充上一失败状态。插件包不包含宿主二进制，这些宿主变化需要重新构建并部署 Sub2API 才会生效；插件不依赖它们。未改变 WS 超时、账号调度或重试策略。

0.1.10 发行 ZIP 中的 `sub2api-bps-0.1.10-host.patch` 单独包含上述三处宿主文件改动，基于工作区提交 `089fe4ea1`。应用到其他 fork 前应检查差异；仅导入 `.s2plugin` 不会应用此补丁。0.1.11/0.1.12 没有新增宿主修改，发行 ZIP 不重复附带旧补丁。

状态查询串行处理，短暂失败后每 5 秒重试只读查询，不重发探测。刷新账号先读取服务器当前配置，保留本窗口未保存的表单；检测到配置变化会提示。宿主没有条件保存接口，跨窗口同时保存仍无法原子协调，请避免同时修改多个设置窗口。

## Codex 功能恢复边界

协议桥接能传递客户端已声明的工具及其结果，不能给未声明工具的请求添加本机访问能力。提示中已明确：存在文件/命令工具时应调用它们检查项目，按客户端指引读取 SKILL.md，不凭目录名推测文件内容。

| 能力 | 本版实现与依赖 |
| --- | --- |
| 读文件、运行命令、编辑文件 | function/custom/namespace 转换与回放；由 Codex 客户端提供工具、执行并处理权限 |
| Skills | 保留 instructions 和工具目录，提示通过工具读取 SKILL.md；发现、安装和执行环境仍归客户端 |
| Subagent | 若客户端将其声明为上述 callable 类型，可按同样信封桥接；是否启用由客户端配置及当前用户授权决定 |
| MCP / Apps | 以 function/custom 暴露时可进入目录；原生 mcp、tool_search 等服务端工具尚未映射 |
| 本机预览 | 桥接已声明的文件打开、浏览器或执行器工具；queued 只代表提交请求。实际渲染、查看与权限仍由 Codex 客户端负责 |
| 输入图片 / 识图 | 用户消息附件上传；工具结果内嵌图片的 inline/附件回退；旧缓存重传与显式缺图提示。用户曾反馈基础识图成功，本版新增容错待实测 |
| 生成 / 编辑图片 | 0.1.12 适配客户端 image_gen.imagegen 使用的独立 Images 接口；需配置 provider 头并开启宿主分组生图。Responses hosted image_generation 未映射 |
| 原生搜索、计算机、独立 compact/input_tokens、服务端会话 | 尚未接入，不能宣称完整恢复 |

同账号/模型/题目/effort 的多轮质量比较仍应单独进行。HTTP 200、返回模型名、一次识图或工具探测通过，都不等于全部 Codex 能力或模型质量通过验收。

### 预览失败如何定位

用户报告“本地服务启动与浏览器本地文件访问均被拦截”，但未提供对应任务或原始工具结果。这句话是模型总结，不能直接作为策略拒绝的证据。静态审查未发现插件阻止浏览器或本地监听的逻辑。0.1.14 增加的是调用指引和诊断，不能据此宣称已修复这次具体故障。

升级后清空诊断，在目标 Codex 任务中请求一次预览，结束后刷新并查看本次新增的多条请求：

1. “输出工具明细”列出模型生成并经插件转换的工具类型/名称，最多 16 项；工具生成并不证明已在客户端执行。`functions.exec` 一类执行器内部的工具由其完整 description 描述，目录没有单独列出浏览器工具并不代表不可用。
2. 下一轮的“客户端工具回传链”取最后一条 user 消息之后的最近 16 个调用或结果，显示原始 input 索引。结果只与本请求中唯一且类型匹配的调用关联，不显示 call_id。清空仅删除插件摘要，新请求携带的历史仍可能包含清空前的调用。
3. 每个结果最多摘录 8 个机器字段：枚举 `status`、布尔 `isError/is_error`、整数 `exit_code`。仅检查结果对象、完整 JSON 对象字符串及 MCP/Responses 文本载体，限制递归和解码字节数；不保存正文、路径、URL 或错误文案。没有识别出字段、没有下一轮回传、纯文本错误或执行器聚合输出，都不能被判定为成功或策略拦截。
4. `queued/accepted` 只是打开请求的回执，`completed/exit_code=0` 也不能证明页面已渲染。模型需要按客户端工具文档取得实际查看结果，才可称预览已验证；如果确实被拒绝，应说明具体工具、操作和原始原因。插件不修改审批规则，也不把 Excel 的运行环境限制套到客户端工具上。

依据：[官方 Browser 文档](https://developers.openai.com/codex/browser/)、[官方权限与审批说明](https://developers.openai.com/codex/agent-approvals-security/)，以及客户端本次声明的工具说明。具体工具可用性与权限以目标设备为准；不要仅凭模型总结修改全局权限。

## 启用 Codex 生图与改图

最新 [Excel bridge 0.4.6](https://github.com/Kaixxrua/excel-codex-bridge/tree/66c41df941fb1a963801964c75ff24b4a19e93f2) 补齐了此前固定参考版本没有的实现：Codex 客户端通过 provider 的 `x-openai-actor-authorization` 头启用本地 `image_gen.imagegen`，执行器向 provider 的 `/images/generations`、`/images/edits` 发送独立请求。这与 Responses 的 hosted `image_generation` 是两条不同链路。0.1.11 的“尚未接入”结论仅代表当时版本；0.1.12 已按新参考移植独立接口。

1. 先在服务器停用旧版、导入并启用 0.1.19，保持 BPS 账号白名单。账号所属分组必须允许生图，并能将 `gpt-image-2` 调度到选中的 BPS OAuth 账号；不要把这个图片模型映射成文字模型。插件“允许的模型”是 Responses 文字模型列表，无需为生图追加 `gpt-image-2`。宿主账号的图片工具策略应选择“继承/允许”，不能为“拦截”。
2. 修改发起任务的那台电脑的 Codex provider 配置。Windows 为 `%USERPROFILE%\.codex\config.toml`，macOS 为 `~/.codex/config.toml`。在当前 provider 表中合并下面一行；`custom` 应与文件中的 `model_provider` 值一致，保留原 `base_url`、鉴权及其他请求头，不要重复创建同名表或重复键。完整说明见 [codex-imagegen.example.toml](codex-imagegen.example.toml)。

   ```toml
   [model_providers.custom]
   http_headers = { "x-openai-actor-authorization" = "excel-codex-bridge" }
   ```

3. 如果曾显式禁用 `[features].image_generation`，将其改为 `true`。完全退出 Codex（包括 Windows 托盘）并重新打开，再新建任务。由 Cockpit 等工具管理配置时，在其当前 provider 中合并相同请求头。
4. 清空插件诊断，然后请求“生成一张蓝鲸图片，普通不透明背景”，或拖入图片请求修改。工具实际执行后应新增 `/images/generations` 或 `/images/edits` 记录。客户端负责保存与显示图片；插件只转发原 Images JSON 响应。原有随机数字图按钮验证的是识图，没有自动生成图片来消耗额度。

该请求头的值是作者使用的客户端标记，不是 token，也不产生新授权；本插件构造 BPS headers 时不把它发送到上游。图片接口仍使用宿主提供的 OAuth Bearer、ChatGPT account ID 和账号代理，不需要新增 `OPENAI_API_KEY`。服务器插件不能替 Windows 客户端修改配置；只升级插件而不配置客户端，仍可能看不到生图工具。

实现沿用作者的字段集合：`prompt`、`model=gpt-image-2`、`output_format=png`、`background=auto/opaque`、`quality=auto/low/medium/high`、`size=auto/1024x1024/1536x1024/1024x1536/1280x720`，可选整数 `n=1..3`。不自动换模型，不使用文字模型白名单校验图片模型。改图接受 `images[].image_url` 的 base64 data URL；转为一张图的 `image` 或多张图的 `image[]` multipart 文件，名称 `picture-N.png/jpg/gif/webp`，保留原字节。仅使用已支持的 JPEG/PNG/GIF/WebP MIME，不下载远程 URL 或跨通道 file_id。

透明背景、非 PNG 输出及范围外参数明确报错。参考没有实现图片流式或 mask，本插件额外拒绝这两种显式请求，避免静默忽略；其他未列出的字段不向 BPS 发送。当前独立图片请求沿用 8 MiB 请求体、32 MiB 响应体和配置的超时限制（默认 600 秒），小于参考项目 64 MiB 的请求限制。插件自身不重试、不缓存生成结果、不主动把请求或成功图片正文保存在诊断中；HTTP 错误保留状态、Retry-After 与错误字段原文，原文可能含上游回显。宿主既有的跨账号调度及图片端点 404/405 回退仍由宿主控制，本轮未改动；需要限定 BPS 路径时，请使用仅含选中 BPS 账号的分组。

本工作区宿主已实现 `/v1/images/generations` 与 `/v1/images/edits` 的 OAuth 直调，`gpt-image-2` 会以独立图片端点经过插件。若服务器 fork 仍把图片请求转成带 hosted 工具的 `/responses`，则需要先升级该宿主能力，不能由插件猜测还原原图片请求。只出现读取 `SKILL.md`、没有生图工具调用，优先核对客户端头与开关；工具调用后无 Images 记录，检查宿主分组权限、模型调度及插件实例；有记录时再看明确的 BPS 拒绝原因。

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
