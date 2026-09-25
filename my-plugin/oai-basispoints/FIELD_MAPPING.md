# 字段处理与源码依据（0.1.8）

这里的“有依据”指参考仓库已实现该行为，不代表本插件已在用户桌面端或 BPS 实机验证。用户已要求自行验收，本次不运行测试或探测。

## 固定参考版本

- CPA：[`f4a2563`](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/tree/f4a2563647f807f5bc08e725fa933bcba28d2d11)，v0.1.8，MIT；用于本版附件上传、工具限制和回放、上下文配置修正。许可证随包附带。
- Excel bridge：[`b2d6f25`](https://github.com/Kaixxrua/excel-codex-bridge/tree/b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a)，Unlicense；其 images.py 采用可抓取图片 URL，本插件选择 CPA 的同源附件方案，不引入临时公网图床或静默删图。
- Codex additional_tools：依据 [OpenAI 官方工具搜索文档](https://developers.openai.com/api/docs/guides/tools-tool-search#add-tools-at-a-specific-point-in-the-input)、[本工作区宿主 Lite 转换](../../backend/internal/service/openai_responses_lite_tools.go)及 [EffectiveResponsesTools / custom exec 适配](../../backend/internal/pkg/apicompat/chatcompletions_responses_bridge.go)。两个参考的上述提交均仅收集顶层 tools，本版额外适配的是客户端/宿主入口，不宣称 BPS 原生支持该载体。
- 0.1.3 已有字段映射依据仍保留下方旧版链接（CPA `05b2d97`、Excel `dca684d`）；新增/变化行为以下表及“0.1.4 新增依据”为准。

这些项目的核心是将请求送入 BPS 的 Excel 客户端通道：复用可用的 ChatGPT OAuth 身份，发送 Excel 客户端 headers、显式 model、顶层 reasoning_effort，再把客户端工具通过 `run_officejs` 信封桥接。它们不能通过本地参数证明服务端模型质量；也没有把任意账号转换成新权限的办法。

## 输入与历史

依据：[Excel `translate_input_items` / `_normalized_tool_output`](https://github.com/Kaixxrua/excel-codex-bridge/blob/dca684d6afd711620fc471e7c00fe10abbc64c8f/src/excel_codex_bridge/excel_upstream.py#L1264)、[CPA `translateInputItems`](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/05b2d97efa1bd117da6bd4d362d6e88f8e483680/internal/basispoints/protocol.go#L266)。本插件实现位于 `internal/basispoints/replay.go`。

| 字段/项目 | 本版处理 |
| --- | --- |
| input 字符串 | 转成 user message；input 数组中的非对象项忽略 |
| 普通 message、未知 input.type | 整个对象保留，包括缺省 type、role、id、status、phase、content 和未知扩展字段；不统一转成纯文本 |
| 嵌套 content | 保留图片、文件、音频、annotations 及其他未知内容块；默认附件模式单独处理 user message 的 data URL 图片，保留原图与 detail；不取回另一通道的 file_id |
| 顶层 text/input_text/output_text | 保持原对象，不猜测 role 或改成 message |
| internal_chat_message_metadata_passthrough | 从客户端 input 项的顶层删除，避免每轮私有 turn_id 破坏提示缓存；不递归删除同名业务字段 |
| reasoning | 有非空 encrypted_content 时发送 type、summary:[]、encrypted_content；无加密内容时过滤 |
| item_reference | 按两仓库规则过滤，不尝试网络拉取或 KV 解析；必须提交完整会话历史 |
| additional_tools | 收集 tools 中的 function/custom/namespace，保留原位置转为 developer 中继目录；原始 carrier 不送 BPS。相同叶子定义去重、冲突报错；本轮 tool_choice 限制同时作用于这些目录 |
| compaction、compaction_summary、compaction_trigger | 普通透传，不删除 id/call_id，不额外检查加密内容，不重排位置 |
| function/custom 调用 | 插件句柄恢复 KV 中的完整原生 item；外部完整历史按 CPA/Excel fallback 构造历史 run_officejs 调用，保留 namespace 与原始 payload；JSON 解码使用 UseNumber 保留大整数精度 |
| function/custom 结果 | 保留 output 结构及扩展字段，恢复原生 call_id、function_call_output 类型和 fc_ ID，按 CPA v0.1.8 删除客户端 name/namespace；null 或空白字符串补为参考项目的成功占位文本 |

工具回放保留多账号隔离：本插件句柄绑定宿主账号、会话、模型和随机句柄，校验过期及调用内容，缺失或过期不会转入 fallback。0.1.8 对非插件句柄接入参考项目的历史 fallback：只消费本请求提供的完整调用及配对结果，不跨账号读 KV、不发起旧工具执行。不以当前工具目录或 tool_choice 重新限制已经执行的历史（对应 Excel 的历史处理）；它们仍限制新输出。缺失/重复/乱序结果、无效参数或异常插件标记仍拒绝。跨模型的加密状态与跨通道文件 ID 不在此修复范围。

## 顶层请求

依据：[Excel `prepare_responses_body`、`_cache_key`、`_agent_turn_state`](https://github.com/Kaixxrua/excel-codex-bridge/blob/dca684d6afd711620fc471e7c00fe10abbc64c8f/src/excel_codex_bridge/excel_upstream.py#L1474)、[CPA `prepareResponsesBody`](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/05b2d97efa1bd117da6bd4d362d6e88f8e483680/internal/basispoints/protocol.go#L459)。实现位于 `internal/basispoints/protocol.go`。

| 字段 | 本版处理 |
| --- | --- |
| model | 保留宿主传入的允许模型原名；默认含 gpt-6-astra。宿主已经处理模型别名，插件不加后缀 |
| model_selection、store | 固定 explicit、false，与参考实现一致 |
| stream | 沿用客户端布尔值，不再强制上游 stream:true |
| instructions | 变为前置 developer message；工具目录同样置于历史之前 |
| reasoning.effort / reasoning_effort | 转为顶层 reasoning_effort。保留既有严格策略：未知值报错、不降低强度；max→xhigh，ultra 需开启。两处冲突报错 |
| prompt_cache_key | 采用原值（去首尾空白），不改成插件 hash；缺省依次尝试 promptCacheKey、session_id、sessionId、client_metadata.session_id/sessionId，与 Excel 一致 |
| context_management | 按 CPA v0.1.8，缺省/null/空数组省略，其他显式值透传给上游校验；不再强制默认 200000 |
| service_tier | 按 CPA v0.1.8，客户端显式提供时透传 |
| metadata | 保留字符串、数值、布尔标量，转为字符串，key/value 按 64/512 字符截取；嵌套对象/数组不转发。布尔值用 true/false（CPA 行为） |
| metadata.task_id / turn_id / agent_iteration | 按 Excel 的 setdefault 语义保留调用方已有值；缺省采用确定性 UUIDv5 task/turn 和当前用户轮的工具结果数+1。CPA 会覆盖这些值，本插件选择 Excel 的保留规则 |
| metadata.bps_tools_version_id | 作为调用方标量 metadata 保留，不另加工具版本发现请求 |
| tools / tool_choice | 顶层及 input.additional_tools 的 function/custom/namespace 转入工具目录，不直接塞进 BPS tools；none 禁用，required、指定 function/custom、allowed_tools 过滤与要求由本插件校验；独立 namespace group choice 尚不支持；本轮工具限制不改变 KV 历史回放 |
| parallel_tool_calls | false 时提示并强制最多一个调用；其他情况允许多个独立信封，全部校验/存储后才发客户端事件；此字段不直接进入 BPS body |
| previous_response_id / conversation / background:true | 参考实现未提供所需的状态恢复或后台接口；本插件明确拒绝，避免无声丢失会话状态 |
| max_output_tokens / temperature / top_p / text / truncation / include / 其余顶层字段 | 参考请求构造中不发送，本版同样不发送。这些选项在 BPS 通道不生效；不把“原样转发”误当作上游已支持 |

请求 JSON 的键顺序和哈希序列化使用 Go 的确定性编码；不声称跨语言 UUID 与参考项目逐字节相同。task/turn 是协议标识，授权隔离仍由宿主和本插件 KV 机制处理。

## 响应和 SSE

依据：[Excel `excel_tool_stream_transform`](https://github.com/Kaixxrua/excel-codex-bridge/blob/dca684d6afd711620fc471e7c00fe10abbc64c8f/src/excel_codex_bridge/excel_stream.py#L90)。实现位于 `internal/basispoints/sse.go`、`tools.go`。

| 项目 | 本版处理 |
| --- | --- |
| 普通 SSE | 不再用事件名白名单拒绝新类型；保留事件中的原字段，普通文本增量不再要求本地先收到 output_item.added |
| 普通 output item | 不再用 type 白名单过滤；保留 content、annotations、phase 及扩展字段。reasoning 缺少 summary 时仅补空数组，不生成虚构的推理文本 |
| function/custom 事件 | 原生工具名和参数暂存，只有终态严格校验的 run_officejs 信封能变成客户端工具事件；每信封一个工具，允许并行时可有多个，重复 ID 拒绝；不放行未声明原生工具 |
| response.completed/failed/incomplete | 保留终态外层扩展字段和 response 中的 usage、model、ID、incomplete_details 等；移除非工具 item 的全对象相等要求。原生工具身份与参数的一致性仍校验 |
| sequence_number | 工具事件被重建后重新连续编号；不能同时承诺原 sequence_number 不变 |
| JSON 响应 | 非流客户端收到转换后的 JSON；流客户端遇到 JSON 回包时保留既有桥接。未知内容保留在 item/终态，不猜测新的内容增量协议 |
| failed、incomplete、提前 EOF | 不伪造 completed，不释放失败响应中的原生工具 |
| 错误 | HTTP 状态码和限流/request ID headers 保留；客户端错误附诊断 ID，后台额外提取受限、脱敏的上游校验字段。原始错误正文、failed.error 和流内错误不直接转发。工具回放错误仅展示 input 序号和 type |

## 0.1.4 新增依据

- [CPA attachments.go](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/f4a2563647f807f5bc08e725fa933bcba28d2d11/internal/basispoints/attachments.go)：同源 attachments、multipart file、原 MIME/字节、openai_file_id 替换、512 条摘要缓存。本插件使用自己的 Go HTTP/代理/取消机制，未照搬 CPA 专用 host.http.do RPC。0.1.5 只缓存成功结果，不合并在途上传，避免客户端取消相互影响；非 2xx 的状态和白名单响应头保留，正文解析只影响诊断。task/turn 在替换图片前生成。
- [CPA protocol.go](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/f4a2563647f807f5bc08e725fa933bcba28d2d11/internal/basispoints/protocol.go)：callableClientToolSpecs、clientToolCallRequired、clientToolProtocolInstructions、translateInputItems、prepareResponsesBody、extractNativeClientToolCall、transformResponseBody 对应工具筛选、命名空间、客户端文件访问提示、custom 的 args 字符串、结果字段清理及并行限制。
- [CPA CHANGELOG.md](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/f4a2563647f807f5bc08e725fa933bcba28d2d11/CHANGELOG.md)：v0.1.8 图片和上下文策略修正。
- [Excel images.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a/src/excel_codex_bridge/images.py)：图片 URL 适配是另一方案，未在本插件使用。

图片/工具探测和最近请求诊断是本插件的观测功能，不是新的 BPS 协议：探测使用相同 prepare/upload/relay/KV/restore 路径；0.1.6 的工具探测将 namespaced custom 声明放在 additional_tools，结果由本地模拟，图片答案仅来自生成图像。它们只在用户显式点击时执行，Health 和 ApplyConfig 不发探测请求。最近请求统计顶层和 additional_tools 的声明来源，并在中继转换之前记录终态原生工具类型/有界名称，供判断转换失败；不记录参数。

0.1.7 的图片路由按钮通过本地 gRPC 帧适配器进入 Forward，构造图片消息＋additional_tools 模拟桌面形态；不会新增 HTTP 入口或客户端工具执行器。图片请求独立历史记录转换前后字段形状和两阶段 HTTP，非 2xx 的 JSON 错误仅抽取白名单校验字段；不采集原始请求或完整错误体。原有图片上传协议与 detail 策略保持不变，定位版本不代表 400 根因已确认。

## 0.1.8 用户实测后的修正依据

- 真实记录 `991cfab37f3b5843743cb35fafdc45a1`：JPEG data URL 转 file_id 后 Responses HTTP 400，允许扩展名为 `.jpeg/.jpg/.png/.gif/.webp`，实际识别 `none`。记录没有保留原始上传文件名，不能证明服务器当次用了哪一扩展名。
- 本地 Go 1.26.4 / darwin-arm64 的 `mime.ExtensionsByType("image/jpeg")` 实际首项为 `.jfif`；CPA 和旧插件都用列表首项。0.1.8 改为四类 MIME 的固定合法扩展名，图片字节不变；这是已确认的主机依赖问题，真实 BPS 是否由此恢复需要用户复测。
- 附件仍使用 CPA 的 multipart `file` 与 `openai_file_id`，没有在 Responses 的 `input_image` 擅自添加 MIME/filename。缓存键包含上传格式与扩展名；缓存只在进程内存，启动新实例清空。
- 附件文件名/MIME/长度摘要与 JPEG/PNG 探测选择属于本地观测及生成样本，不是新增上游协议。原有 PNG 探测通过不能覆盖 JPEG 路径。
- 旧工具历史依据：[CPA `fallbackTransportCall` / `translateInputItems`](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/708082da2f851569984de395d25405e61c2bbc34/internal/basispoints/protocol.go)、[Excel `_fallback_transport_call` / `translate_input_items`](https://github.com/Kaixxrua/excel-codex-bridge/blob/b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a/src/excel_codex_bridge/excel_upstream.py)。用户确认在切换通道/模型或继续旧任务后遇到非插件句柄错误，本版取消对完整外部历史的无条件拒绝；不把缺失参数补为空对象，也不为孤立结果编造调用。

这里只承诺字段处理与所列源码行为对应；不增加原生搜索/计算机/MCP/tool_search 映射、独立 compact/input_tokens 端点、凭据交换或新的后台操作。客户端未声明工具、非法 JSON、失效回放等严格错误仍保留。工具结果中的图片目前只透传；没有根据用户消息附件的规则猜测另一种上传协议。
