# Excel bridge 容错对照（0.1.15）

0.1.15 依据 ranxi2001/sub2api `f671a8d30c34706d8526accadf6a6ad5f40f855e` 新增显式 CUSTOM/FUNCTION_CODE 原文传输，并将完整响应的信封错误收尾为带 ID 的 response.failed；没有自动模型纠正或额外上游请求。源码函数对照、两条路由及未移植范围见 [HOST_INTEGRATION.md](HOST_INTEGRATION.md)。以下历史版本说明仍保留；新调用优先原文模式，旧 JSON 信封继续兼容。

参考固定为 [Excel bridge 0.4.6 / 66c41df](https://github.com/Kaixxrua/excel-codex-bridge/tree/66c41df941fb1a963801964c75ff24b4a19e93f2)，并与此前 `b2d6f25` 对照。另保留 CPA `708082d` 的附件、回放和工具约束依据。此表覆盖与本插件请求链路有关的容错；Python 启动器、Excel 登录和 Windows 凭据存储不移植到服务端 transport 插件。

“已实现”表示代码及编译检查已完成，**不表示运行测试或真实 BPS 验收通过**。用户要求自行测试，本轮没有发送账号请求。用户诊断 `84d0e06a66fcf09c77369feb61519acb` 显示外层 arguments 可解码，内层 code 为 invalid_escape；未取得原始 code，不能确认具体字符。反斜杠兼容在 `b2d6f25` 已存在，属于补齐旧插件遗漏，并非作者在 0.4.6 首次增加。

## 工具信封与回放

0.1.14 额外增加客户端工具调用/结果摘要，以及执行器嵌套工具和预览回执的指引。这是针对用户新反馈的本地诊断增强，不是 Excel bridge 新增了预览协议；不增加上游字段、不改工具参数或客户端权限。具体边界见 README 的“预览失败如何定位”。以下 0.1.13 移植行为保留。

源码：[excel_upstream.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/excel_upstream.py)。

| 参考函数/行为 | 插件处理与边界 | 本地实现 |
| --- | --- | --- |
| `_decode_transport_code` 对象/JSON 字符串 | 接受两者，JSON 数组、null、不完整 JSON 不作为工具信封 | relay_envelope.go |
| 代码围栏、赋值、外围文字 | 提取第一个完整对象，不执行外围脚本；额外对象、重复键、损坏外层继续拒绝，不从损坏外层捡取内部对象 | relay_envelope.go |
| `_repair_invalid_json_backslashes` | 仅 code 字符串内不合法的 JSON 反斜杠加倍，例如 `\(` 保留为命令中的反斜杠及括号；合法 `\n`、`\t`、`\f`、`\uXXXX` 等不改。不会猜测 Windows 路径本来想要何种转义 | relay_envelope.go |
| `_transport_envelope` 重复包装 | 最多解开两层 run_officejs / functions.run_officejs；外层和嵌套 arguments 仍需合法 JSON。解析后继续校验目录、字段、Schema、tool_choice | relay_envelope.go、tools.go |
| function arguments 的对象或 JSON 字符串 | 原有兼容保留；UseNumber 保留大整数，重复键拒绝 | tools.go、config.go |
| custom 原始 input | 原有逐字保留，补强提示避免放入 arguments.code/patch；空字符串与缺失/null 区分 | tools.go、replay.go |
| `_client_call_from_native` | 已声明且类型匹配的直接 function/custom 可转换；支持旧 codex_client__ 名称。不能凭相似后缀匹配一个未声明工具 | native_tools.go、tools.go |
| `_normalize_native_function_arguments` | update_plan 的 description/title→step、summary→explanation 和状态别名按参考映射；随后仍校验客户端 Schema | native_tools.go |
| `_restore_native_function_arguments` | 原生记录命中时完全使用原件；完整历史重建 update_plan 时恢复 summary/id/description/status/result 格式 | native_tools.go、history.go |
| `_normalized_tool_output` plan | 按参考将 Codex 的计划显示结果转成 BPS 的 status:ok，避免反复规划；只是历史协议格式，不是插件实际执行了计划步骤 | native_tools.go、replay.go |
| unsupported run_officejs 结果 | 替换为参考的一次正确封装指导，模型自行决定后续调用；插件不重发同一个坏信封 | native_tools.go |
| 空输出、结果类型和 ID | 空白/null 输出用既有明确占位；relay-backed custom 结果转 function，直接 native custom 保持 custom；function 输出使用规范 fc_ ID，清理 name/namespace | replay.go |
| 原生调用持久化与丢缓存恢复 | 原件按账号/会话/模型保存；KV 未命中时只转换客户端完整调用及结果。损坏、明确过期、内容不符或 KV 读取错误不当作普通 miss；不重新执行旧工具 | replay.go、history.go |
| 多个 native calls | 按顺序保留有效调用，跳过不兼容兄弟；全部无效仍失败，不向客户端释放未转换原生工具 | tools.go |
| `parallel_tool_calls:false` | 仅保留第一个有效调用，其余不执行；重复 ID 不借此隐藏；所有保留调用存储成功后才释放 | tools.go、sse.go |
| `_agent_turn_state` | 相邻 function/custom 结果作为同一轮，避免并行多个结果让 iteration 多增；后续新的结果轮再增加 | protocol.go |
| 稳定前置目录/提醒 | 现有 instructions、目录置于前部；additional_tools 的目录保留其引入位置；不向每轮末尾追加会破坏缓存前缀的提醒 | protocol.go、tool_sources.go |
| `_normalize_reasoning_effort` | API 输入支持 trim/lower 和 x-high/extra-high/extra_high，保留 max→xhigh；未知值仍明确失败，不照搬静默 medium 回退 | protocol.go |

## 流式与网络

源码：[excel_stream.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/excel_stream.py)、[sse.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/sse.py)、[server.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/server.py)。

| 参考行为 | 插件处理与边界 | 本地实现 |
| --- | --- | --- |
| 原生工具事件暂存 | 不转发未经校验的参数增量；终态转换并保存后发 added/delta/done | sse.go |
| 跳过调用后的输出顺序 | 为后续文本/推理保留事件，按最终 output 调整 output_index；统一重编号 sequence_number | sse.go |
| `_with_ticks` 静默保活 | 有真实响应 ID 后，静默 15 秒发 in_progress；不发第二个上游请求，取消/结束后停止并关闭读端 | stream_compat.go |
| `completion_from_finished_items` | 缺终态但全部 seen 项 done、索引连续可靠、没有未完成增量时，允许从非 commentary 消息或工具收尾；必须有真实 response ID。半项、仅 commentary/reasoning、重复或缺索引不恢复 | stream_compat.go |
| 恢复完成的计费边界 | 诊断单独记录 completion_recovered；上游没有返回的 usage 不填造，不能声称已收到真实 completed | stream_compat.go、diagnostics.go |
| 无空行的末 SSE block | 完整 JSON 末块仍处理；半个 JSON 不据此收尾。实际读到的字节数限制仍生效，包括 CRLF | sse.go |
| completed 后断线 | 已确认终态后停止消费，忽略尾部断线；failed/incomplete 保留失败终态 | sse.go |
| 非流式 RemoteProtocolError 重试 | 对应 Go EOF/UnexpectedEOF、HTTP/2 帧或连接中断最多一次；不把 JSON/信封错误、普通超时和主动取消视为可重试 | transport.go、stream_compat.go |
| 流式失败 | 已开始的流式响应不重发；只在上述完整 done 条件下本地收尾，否则返回错误 | transport.go |
| reasoning 展示兼容 | 现有 summary/content 文本规范到客户端 summary 格式；encrypted_content 保留，仅加参考的通用完成占位，不生成隐藏推理正文 | tools.go |

## 图片识别与独立生图

源码：[images.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/images.py)、server.py 的 `_send_with_pictures`，以及 [image_generation.py](https://github.com/Kaixxrua/excel-codex-bridge/blob/66c41df941fb1a963801964c75ff24b4a19e93f2/src/excel_codex_bridge/image_generation.py)。

| 参考行为 | 插件处理与边界 | 本地实现 |
| --- | --- | --- |
| 递归图片处理 | input 内各层的 input_image data URL，包括 function/custom 工具结果；字符串内的 JSON 不二次解析为图片 | picture_fallback.go |
| 按顶层 item kind 学习 inline 拒绝 | 工具结果等先 inline，400/422 后逐类改同源附件，message 优先；本插件保留用户已验证的 message 默认附件，无需每次先经历一次已知拒绝 | picture_fallback.go |
| 缓存复用、失效刷新 | 400/422 先忘掉本次复用的旧 ID 并重传；fresh 集合跨本请求各次尝试保留，避免重传新上传图片 | picture_fallback.go、attachments.go |
| 多账号缓存隔离 | 沿用更严格的账号/凭据/端点/格式/内容摘要键，缓存只保存成功结果；并发上传各自拥有取消上下文 | attachments.go |
| 上传网络不可用 | 包括收到 HTTP 头后读响应体中断；本轮后续新图片不继续上传，已有成功缓存仍可读；已观察到的 HTTP 状态保留。单次上传 120 秒并受总请求取消约束 | picture_fallback.go、attachments.go |
| 上传错误/无文件 ID | 使用明确缺图占位，诊断保留上传尝试、HTTP 和脱敏原因；支持格式仍限 JPEG/PNG/GIF/WebP，不伪装未知 MIME | picture_fallback.go、attachments.go |
| 最终仍拒绝图片 | 插入 `[image content omitted: ...]`，计数并在 UI 提示；主动图片路由探测不能将文本降级视为识图通过 | picture_fallback.go、image_route_probe.go |
| 重试条件和终止 | 仅 Responses 400/422；每轮推进旧缓存刷新、按类型上传或最终省略，额外次数不超过图片种类数+2，且受总请求超时约束；新鲜附件不会反复上传 | picture_fallback.go、transport.go |
| 重试身份 | 每次从同一原始 input 重建图片载体；task_id、turn_id、iteration、模型、detail 不因重试改变 | protocol.go、picture_fallback.go |
| 其他传输方式 | image_transport=passthrough 不做图片回退；URL、现有 file_id 不下载/不跨通道恢复；独立图片探测保留一次直接上传流程作为对照 | transport.go、probe.go |
| 重试诊断 | 保留最多 16 次回退摘要：次数、动作、HTTP、Request ID、脱敏原因；新尝试在上传前清空当前 Responses 状态，防止取消后展示前次 HTTP | diagnostics.go、transport.go、UI |
| 嵌套图片诊断 | 递归计数，摘要最多 16 项；任意业务键用 `*` 代替，不记录图片、URL、file_id；工具声明内示例不作为图片输入 | image_diagnostic.go |
| 独立 Images 生图/改图 | 0.1.12 已有 JSON generation、multipart edit、固定 gpt-image-2/PNG、provider 头说明；不套用 Responses 的上传/省略/重试 | image_generation.go |
| Images HTTP 200 异常体 | 新增识别 error、空 data 或仅 URL 响应；按宿主 b64_json 能力计数并标记失败，原 JSON 保留交给宿主，不能只凭 200 报出图成功 | image_generation.go |

## 有意保留的差异及宿主职责

- 参考中的 `<codex_tool_call>` 文字标记是旧代理协议，作者注释说明新调用使用 run_officejs。本插件不从普通回复文字创建新的可执行调用；完整旧工具调用及结果仍可走历史转换。此旧行为没有无声遗漏，也不应为了容错而让引用示例变成命令。
- 不照搬“捡出损坏外层里的内部对象”、多对象只取一个、缺 ID 随机补身份、损坏 arguments 改为空对象、全部转换失败时放出原生调用、无 ID 也合成 completed 等行为。继续保护参数完整性、原生身份、目录/Schema、回放账号隔离、请求/响应上限。
- 用户原样选择的模型不会换成参考项目的默认模型；未知 effort 不降级。保留 CPA 的显式 compaction 配置规则，不自动加入 200000 阈值。
- Excel 登录/会话捕获、Windows DPAPI、Python 本地监听与 loopback 检查、压缩 HTTP 入站、鉴权、models/capabilities、配置备份及启动器升级由客户端或 Sub2API 宿主承担。本插件不新建监听端口，不从服务器读取用户桌面凭据，不自动修改 Codex 配置。
- Sub2API 负责账号令牌刷新、代理配置、入口限流/调度和图片权限。插件不会把参考 server 的本地重试扩展为跨账号切换，也不会修改宿主已有重试策略。原生 hosted image_generation、搜索、MCP/tool_search、独立 compact/input_tokens 仍未实现。

## 回归源码与交付检查

已补或更新 `relay_envelope_test.go`、`native_tools_test.go`、`stream_compat_test.go`、`picture_fallback_test.go`、`transport_compat_test.go` 及 `tests/ui.test.cjs`。用例覆盖反斜杠原文、对象提取/拒绝歧义、原生回放类型、混合与串行工具、结果轮次、完整项恢复与半项拒绝、SSE 索引、保活取消、缓存重传/省略、工具结果图片、网络不可用仍用缓存、HTTP 重试范围及 Images 假成功。Go 用例只编译，JS 只做语法检查，没有执行用例。

构建和签名状态以 [VALIDATION.md](VALIDATION.md) 为准；生产恢复情况由用户升级后验收。
