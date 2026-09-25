# 字段处理与源码依据（0.1.14）

这里的“有依据”指参考仓库已实现该行为，不代表本插件已在用户桌面端或 BPS 实机验证。用户已要求自行验收，本次不运行测试或探测。

0.1.14 不增加 BPS wire 字段。仅补充 relay 提示，以及本地 `output_tools`、`client_tool_history`、`client_tool_history_count` 诊断字段：转换后名称、当前用户轮次的最近调用/结果索引和受限机器状态。它们不会发送给 BPS，不解析执行器脚本或自然语言错误，不改变原始结果。预览指引依据客户端工具说明及 README 链接的官方浏览器/权限文档，不宣称参考仓库实现了新的预览协议。

## 固定参考版本

- 本轮复核的 HEAD：CPA `708082da2f851569984de395d25405e61c2bbc34`（v0.1.9），Excel bridge `66c41df941fb1a963801964c75ff24b4a19e93f2`（0.4.6）。Images 接口依据见“0.1.12”；0.1.13 按该 Excel 提交补齐 Responses 工具、流式及图片容错。完整函数对照、未照搬的历史行为及宿主边界见 [COMPATIBILITY.md](COMPATIBILITY.md)。
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
| 嵌套 content/output | 保留未知内容块；附件模式递归识别 input_image data URL。message 默认附件，其他类型先 inline，被 400/422 拒绝后改附件；解码/上传失败或最终仍被拒时插入明确缺图占位并诊断。普通 URL/file_id 不转换 |
| 顶层 text/input_text/output_text | 保持原对象，不猜测 role 或改成 message |
| internal_chat_message_metadata_passthrough | 从客户端 input 项的顶层删除，避免每轮私有 turn_id 破坏提示缓存；不递归删除同名业务字段 |
| reasoning | 有非空 encrypted_content 时发送 type、summary:[]、encrypted_content；无加密内容时过滤 |
| item_reference | 按两仓库规则过滤，不尝试网络拉取或 KV 解析；必须提交完整会话历史 |
| additional_tools | 收集 tools 中的 function/custom/namespace，保留原位置转为 developer 中继目录；原始 carrier 不送 BPS。相同叶子定义去重、冲突报错；本轮 tool_choice 限制同时作用于这些目录 |
| compaction、compaction_summary、compaction_trigger | 普通透传，不删除 id/call_id，不额外检查加密内容，不重排位置 |
| function/custom 调用 | 插件句柄优先恢复同范围 KV；未命中或外部完整历史按 CPA/Excel fallback 构造历史 run_officejs 输入，保留 namespace 与原始 payload；JSON 解码使用 UseNumber 保留大整数精度 |
| function/custom 结果 | 保留 output 结构及扩展字段，恢复原生 call_id 和实际原生类型；function 结果规范为 fc_ ID，直接 custom 仍是 custom_tool_call_output；删除客户端 name/namespace。空结果补参考占位文本；update_plan 回放为 status:ok；unsupported run_officejs 结果换为一次正确封装指导，不自行执行重试 |

原生工具回放按宿主账号、会话、模型和随机句柄隔离，命中后校验过期及调用内容。0.1.8 为非插件句柄接入完整历史 fallback；0.1.10 将相同处理扩展到合法插件句柄的 KV 未命中，匹配参考项目“rememberedNativeCall 无记录时转换完整调用”的行为。只消费本请求提供的完整调用及配对结果，不跨账号查 KV、不写入伪造原生记录、不发起旧工具执行。明确过期/损坏的已存记录、存储错误、孤立/重复/缺失结果继续拒绝。当前工具目录及 tool_choice 约束新输出，不重新限制已执行历史。0.1.9 的 custom input 类型、namespace 类型、异常多重前缀检查保留。跨模型加密状态与跨通道文件 ID 不在此修复范围。

## 顶层请求

依据：[Excel `prepare_responses_body`、`_cache_key`、`_agent_turn_state`](https://github.com/Kaixxrua/excel-codex-bridge/blob/dca684d6afd711620fc471e7c00fe10abbc64c8f/src/excel_codex_bridge/excel_upstream.py#L1474)、[CPA `prepareResponsesBody`](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/05b2d97efa1bd117da6bd4d362d6e88f8e483680/internal/basispoints/protocol.go#L459)。实现位于 `internal/basispoints/protocol.go`。

| 字段 | 本版处理 |
| --- | --- |
| model | 保留宿主传入的允许模型原名；默认含 gpt-6-astra。宿主已经处理模型别名，插件不加后缀 |
| model_selection、store | 固定 explicit、false，与参考实现一致 |
| stream | 沿用客户端布尔值，不再强制上游 stream:true |
| instructions | 变为前置 developer message；工具目录同样置于历史之前 |
| reasoning.effort / reasoning_effort | 转为顶层 reasoning_effort；API 输入规范大小写/空白，参考别名 x-high/extra-high/extra_high 及既有 max→xhigh；ultra 需开启。规范化后冲突或未知值报错，不静默降为 medium |
| prompt_cache_key | 采用原值（去首尾空白），不改成插件 hash；缺省依次尝试 promptCacheKey、session_id、sessionId、client_metadata.session_id/sessionId，与 Excel 一致 |
| context_management | 按 CPA v0.1.8，缺省/null/空数组省略，其他显式值透传给上游校验；不再强制默认 200000 |
| service_tier | 按 CPA v0.1.8，客户端显式提供时透传 |
| metadata | 保留字符串、数值、布尔标量，转为字符串，key/value 按 64/512 字符截取；嵌套对象/数组不转发。布尔值用 true/false（CPA 行为） |
| metadata.task_id / turn_id / agent_iteration | 保留调用方已有值；缺省确定性 task/turn，连续工具结果视为一轮，iteration 为轮数+1。先由原始客户端输入计算，图片改写和重试不改变身份 |
| metadata.bps_tools_version_id | 作为调用方标量 metadata 保留，不另加工具版本发现请求 |
| tools / tool_choice | 顶层及 input.additional_tools 的 function/custom/namespace 转入工具目录，不直接塞进 BPS tools；none 禁用，required、指定 function/custom、allowed_tools 过滤与要求由本插件校验；独立 namespace group choice 尚不支持；本轮工具限制不改变 KV 历史回放 |
| parallel_tool_calls | false 只选第一个有效调用；其他情况保留所有有效调用，跳过坏调用但记录数量，全部无效仍报错。保留调用均通过校验/存储后才发事件；此字段不直接进入 BPS body |
| previous_response_id / conversation / background:true | 参考实现未提供所需的状态恢复或后台接口；本插件明确拒绝，避免无声丢失会话状态 |
| max_output_tokens / temperature / top_p / text / truncation / include / 其余顶层字段 | 参考请求构造中不发送，本版同样不发送。这些选项在 BPS 通道不生效；不把“原样转发”误当作上游已支持 |

请求 JSON 的键顺序和哈希序列化使用 Go 的确定性编码；不声称跨语言 UUID 与参考项目逐字节相同。task/turn 是协议标识，授权隔离仍由宿主和本插件 KV 机制处理。

## 响应和 SSE

依据：[Excel `excel_tool_stream_transform`](https://github.com/Kaixxrua/excel-codex-bridge/blob/dca684d6afd711620fc471e7c00fe10abbc64c8f/src/excel_codex_bridge/excel_stream.py#L90)。实现位于 `internal/basispoints/sse.go`、`tools.go`。

| 项目 | 本版处理 |
| --- | --- |
| 普通 SSE | 不再用事件名白名单拒绝新类型；保留事件中的原字段，普通文本增量不再要求本地先收到 output_item.added |
| 普通 output item | 保留未知项及扩展字段；reasoning 按参考将现有 summary/content 文字规范到 summary_text/reasoning_text 并加显示标题；仅有 encrypted_content 时使用参考的完成占位文字，不解密或编造推理内容 |
| function/custom 事件 | 暂存原生工具；终态校验 run_officejs 信封或已声明、类型匹配的直接原生调用。兼容旧 codex_client__ 名称，update_plan 按参考转换参数/结果。重复 ID、未声明工具不能释放 |
| response.completed/failed/incomplete | 保留终态外层扩展字段和 response 中的 usage、model、ID、incomplete_details 等；移除非工具 item 的全对象相等要求。原生工具身份与参数的一致性仍校验 |
| sequence_number / output_index | sequence_number 连续重编；跳过原生调用时暂存后续事件并按终态 output 重排索引与发送顺序，避免文本/工具错位 |
| JSON 响应 | 非流客户端收到转换后的 JSON；流客户端遇到 JSON 回包时保留既有桥接。未知内容保留在 item/终态，不猜测新的内容增量协议 |
| failed/incomplete | 保留实际失败状态，不释放其中的原生工具 |
| 提前 EOF/传输断开 | 有真实 response ID、可靠连续索引且所有项 done、无未完成增量，末项为非 commentary 消息或工具时可恢复 completed；诊断标记且省略未知 usage。其余提前结束仍失败 |
| 静默与末块 | 实际流开始后每 15 秒静默保活；无空行的完整末块仍解析。终态后的断线不推翻结果，取消时停止保活并关闭读取器 |
| 非流式重试 | 可识别的 HTTP EOF/帧中断仅一次；普通超时、JSON/信封/Schema 错误及流式请求不适用；Responses 图片 400/422 的策略回退单独记录 |
| 错误 | HTTP 状态码和限流/request ID headers 保留；客户端错误附诊断 ID，后台记录来源、信封形态与有界解析类别/偏移、回放计数，不记录工具参数正文；原始上游错误、failed.error 和流内错误不直接转发 |

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

## 0.1.12 独立 Images API 与客户端工具

依据：[image_generation.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/image_generation.py)、[Bridge.images](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/server.py#L262)、[IMAGE_TOOL_HEADER / codex_config](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/codex_config.py#L22)及 [参考请求夹具](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/tests/test_image_generation.py)。

| 环节 | 已移植行为 |
| --- | --- |
| 客户端提供工具 | 在当前 provider 的 http_headers 合并 x-openai-actor-authorization=excel-codex-bridge；保持客户端 image_generation 开关开启。仅提供配置说明，不改用户全局配置或技能 |
| 工具调用 | 已有 function/custom/namespace 中继承载客户端 image_gen.imagegen；执行器向 provider 图片端点发独立请求，不伪装成 hosted image_generation |
| 文生图 | JSON POST 到 BPS /basispoints/api/images/generations |
| 改图 | 客户端 JSON images[].image_url 解码为图片原字节，POST multipart 到 /basispoints/api/images/edits；单图 image，多图 image[]，文件名 picture-N 加明确扩展名 |
| 公共字段 | prompt 原字符串；model 固定 gpt-image-2；output_format 固定 png；background/quality/size 缺省 auto；可选 n 为 1..3 的整数；支持列表与参考 _fields 相同 |
| 身份与代理 | 复用宿主 OAuth、ChatGPT account ID、账号代理，Accept=application/json。provider 的 actor 标记不进入 BPS headers，也不代替 Authorization |
| 响应 | 有界 JSON 对象返回给宿主，保留 data、b64_json、usage 和扩展字段；HTTP 错误保留状态及 Retry-After，插件自身不自动重试；宿主的调度及回退策略保持原样 |
| 诊断 | image_operation、generated_images、upstream_started；生图即使没有输入图片也进入图片历史。只记录数量、字段摘要与状态；错误脱敏覆盖 prompt/images/mask |

本地边界与参考不同处：沿用插件 8 MiB 请求、32 MiB 响应限制，要求有效无重复键 JSON，仅接受既有 JPEG/PNG/GIF/WebP MIME；参考虽未传递 stream/mask，本版明确拒绝其显式使用，避免执行与原请求不同的操作。其他未列出的字段不发送，不扩展透明背景、图片模型、尺寸、URL 下载等未经新参考实现的行为。宿主先于插件处理图片权限、模型映射与账号调度；插件无法恢复宿主已经丢失或改写的字段。

宿主依据：[OAuth 直调](../../backend/internal/service/openai_images_direct.go)、[图片转发](../../backend/internal/service/openai_images_responses.go)、[统一插件出口](../../backend/internal/service/openai_plugin_transport.go)。gpt-image-2 的 generations/edits 在本工作区可进入相同 OAuth transport 插件，不需要另开监听端口或增加执行器。服务器 fork 需具备这条路由；旧宿主将图片转换为 hosted Responses 时不由本版自动猜测还原。

## 0.1.11 诊断历史与生图边界（历史）

- 参考 [CPA iterToolValues](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/708082da2f851569984de395d25405e61c2bbc34/internal/basispoints/protocol.go#L37) 与 [Excel _iter_client_tools](https://github.com/Kaixxrua/excel-codex-bridge/blob/b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a/src/excel_codex_bridge/excel_upstream.py#L237)，本插件仍仅桥接 function/custom/namespace 叶子。原生 image_generation、tool_search 等类型会被统计但不进入目录，本版 UI 将该边界明确显示；不会根据工具名字猜测其生图能力。
- `clear_diagnostics` 是插件本地管理命令，复用 Host UI Bridge v1 config.save/config.test；ApplyConfig 本身不会执行命令。清空不发送 BPS 请求，不改变请求字段、工具回放和图片上传协议。
- 状态新增 `diagnostic_generation` 与 `diagnostics_cleared_at`，只用于清空后的请求归档和 UI 选择同步。清空时更新代次及两个内存历史，旧代次在途请求完成后跳过归档。
- 当时仅依据旧参考评估普通生图通道；新参考提供独立 BPS Images 路径，0.1.12 已替换此前接入方案，见上节。

## 0.1.10 运维反馈修正（历史，信封容错已由 0.1.13 扩展）

- 信封解包依据 [CPA transportEnvelope](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/708082da2f851569984de395d25405e61c2bbc34/internal/basispoints/protocol.go#L623)：最多两层，仍是单工具 JSON 信封。
- 围栏兼容取自 [Excel _decode_transport_code](https://github.com/Kaixxrua/excel-codex-bridge/blob/b2d6f2529b6ffa9f1a630f17b7037ef6dcc0480a/src/excel_codex_bridge/excel_upstream.py#L396) 的较窄子集：仅完整 JSON/无语言围栏，不扫描任意文本中的第一个对象，不修非法反斜杠。解析仍拒绝重复键、多对象、尾随数据及过深嵌套；偏移以当前层解码内容为准。
- 缺失原生记录的完整历史转换对应 [CPA translateInputItems / fallbackTransportCall](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints/blob/708082da2f851569984de395d25405e61c2bbc34/internal/basispoints/protocol.go#L324)。此路径仅使用客户端已提供内容，无法保留原生不透明状态。未命中不再直接推定过期；KV 故障及已找到但校验失败的记录不会降级。
- 工具探测拓展为三轮 custom/function/结果核对，全部使用虚拟工具；生成样本不会在服务器或客户端运行。
- 模型白名单拒绝增加本地配置错误码与来源，不修改模型名或允许列表；Responses 发送阶段与 HTTP/流式转换结果分开展示。
- 宿主 Redis miss 与日志字段为单独源码修改，不属于 BPS 协议；未调整 WS 超时、选号或重试。

本次未取得两条 JSON 解析故障的原始 code 形态，也未取得新回放故障发生前后的范围指纹。兼容路径有参考依据，但不能宣称已复现或已实机解决全部反馈。

## 0.1.9 审查修正（历史）

- `sameCall` 原先使用 `str` 比较 custom input，导致空字符串与缺失/null/错误类型相等；现在要求两侧都为原始字符串。对象、数组、数字等错误类型的 namespace 不再静默退化为无 namespace。
- `replayHandleKind` 原先只剥一个前缀，`call_ctc_bp_…` 可被当作外部历史；现在仅为识别保留标记而逐层剥离已知前缀，并将其判为无效插件句柄，不增加可加载的 KV ID 格式。
- `attachmentReport` 的 HTTP 字段原先跨图片共用且未在新尝试前清空；现在逐次重置，记录尝试次数与逐图 HTTP。UI 将汇总字段标为最近一次上传尝试，区分全缓存命中和后续上传未收到 HTTP 响应。
- 附件非 2xx 改用 `readUpstreamDiagnostic`，与 Responses 共用大小上限、已知请求值/引号内容/URL/不透明串脱敏和采集状态。保留上传状态及限流响应头，不把诊断读取失败改判为未知网络错误。

以上是 0.1.9 当时的审查范围；当时未加入图片重试或工具结果图片上传。0.1.13 依据新 Excel images.py 扩展这些路径，见当前表格及 COMPATIBILITY.md。原生搜索/计算机/MCP/tool_search、独立 compact/input_tokens 和凭据交换仍不在已实现范围。
