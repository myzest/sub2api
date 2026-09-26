# ranxi2001/sub2api 对照与路由边界（0.1.19）

检查日期：2026-09-26。参考仓库：<https://github.com/ranxi2001/sub2api>，production 固定到 `f671a8d30c34706d8526accadf6a6ad5f40f855e`（2026-09-26 11:47:46 +08:00）。这里只报告该提交源码，不推定远端服务已部署它。0.1.15 对照时当前工作区 origin 是 myzest/sub2api，HEAD 为 f62cf9e84，未包含下面整套原生 Excel 适配。0.1.16 开始 HEAD 为 09060f307；0.1.17 开始 HEAD 为 88c8d6953；0.1.18 开始 HEAD 为 4808f43a2。本轮仍只修改独立插件，没有合并或部署宿主。

## 两条入口，不是可随意叠加的修复

- **宿主原生 Excel**：`backend/internal/service/openai_gateway_forward.go` 中，账号 `openai_excel_bps` 开关和 `openai_excel_bps_models`（映射后模型）命中，且没有原生回退原因时，提前进入 `forwardExcelBPS`。`openai_excel_bps.go` 直接通过宿主 HTTP 上游发送 BPS，绕开 OAuth transport 插件。
- **原生能力回退**：`basispoints/route.go` 对部分 hosted 生图/搜索声明或工具选择走原生 Codex，不能认定所有请求都经过 Excel。该路径也不能假定会被插件放行：插件有独立账号白名单。
- **本插件**：`internal/basispoints/transport.go` 由 transport 插件入口调度，再按 route_enabled、account_ids 和允许模型处理。插件诊断只覆盖实际经过插件的请求。

验收时应明确选择一条链路，不把原生 Excel 开关和插件路由同时开启当作效果叠加。验证原生实现时将测试账号移出插件白名单；验证本插件时确保该账号/模型没有提前命中宿主原生 Excel。插件不会自动修改这些设置。

用户诊断 `614dca52d3eb65af0db1825000ea74be` 带 v0.1.14 插件记录，说明当次经过本插件，不能推定以后全部模型仍走同一入口。宿主原生修复不会自动作用于旧插件，反之亦然。

## 采用的源码依据

| 参考文件（backend/internal/service/ 下） | 本插件采用内容 |
| --- | --- |
| basispoints/custom_transport.go | summary=codex2api.custom/完整工具名，code 直接承载 custom input，减少一层 JSON 字符串嵌套。 |
| basispoints/function_code_transport.go | 参数 schema 明确为对象且 properties.code.type=string 时，使用 codex2api.function_code/完整工具名；code 原文与其他参数 JSON（extended_summary）分离，禁止重复 code。 |
| basispoints/catalog.go、request.go | 在目录中逐工具说明传输方式；普通 function 仍使用 JSON 信封。 |
| basispoints/stream.go | 协议转换错误使用 response.failed。本插件保留自己的严格终态检查、真实 response ID、已观察 usage 与诊断 ID，不照搬缺 ID 的最小失败事件。 |

对应实现：raw_transport.go、relay_envelope.go、tools.go、tool_sources.go、history.go、sse.go、transport.go。新增诊断只记录传输模式、长度和类别，不保存代码。原生 KV 命中原样回放；完整 custom 历史重建采用原文标记，普通 function 历史保留兼容信封。

## 未照搬与未验证的边界

- 不执行脚本来提取命令，不把无标记任意 JavaScript 当工具，不根据偏移猜引号。原始坏 code 未取得，不能确认偏移 1133 的具体字符。
- 不增加模型请求来改写参数。取得该 fork 的直接原文协议依据后，撤掉先前拟议、尚未交付的单次模型纠正，发行包不包含该逻辑，不新增此类推理费用或旧工具执行。
- 不移植宿主的临时公网图片 URL、内存缓存、403 自动关闭、计费归一化、structured output 或原生调度；既有同源附件、持久 KV、权限与模型配置不变。
- 显式标记才启用原文模式，外层仍需合法 JSON。完整名称、类型、Schema、tool_choice、原生 ID、请求和回放上限继续校验。原文协议单项上限 1 MiB，仍受现有 256 KiB 回放记录上限约束。
- 功能用例仅编译，未执行真实账号请求、浏览器或部署。减少嵌套转义不等于保证模型永不出错，更不证明模型质量。

## 用户验收

1. 先确认入口；插件路径升级后应产生 v0.1.19 新记录。原生路径没有新插件记录可能正常，勿用旧记录定位新请求。
2. 新任务请求读本地文件，functions.exec 的解析记录应出现 CUSTOM 原文；schema 声明 code:string 的函数可出现 FUNCTION_CODE 原文＋元数据 JSON。普通 function、旧会话仍兼容 JSON。
3. 继续完整会话核对回放。文件读取、浏览器、Subagent 和权限仍由客户端执行；升级不修改客户端权限。
4. 若再出错，核对终态与诊断 ID，HTTP 200 不等于成功。完整响应的信封转换失败改为明确 failed；0.1.16 同时将显式上游 error 作为失败终态正常收尾，保留原文字段。网络中断、取消或无可靠终态仍可能断流。不同宿主版本的错误脱敏/透传规则需用户实测。

### 失败终态与宿主换号边界

另检查了该 fork 的 openai_gateway_passthrough.go::openAIStreamFailedEventShouldFailover：尚未输出语义内容时，未分类的 response.failed 默认可触发换号。插件因此用宿主已有的 invalid_request_error 不可重试分类承载确定的信封协议错误，保留专用 bps_tool_envelope_invalid 码及明确的 BPS 输出错误说明，不归咎客户端输入。宿主可能在对外失败事件中删除 output/usage 等字段，插件端仍保留观察到的终态和用量；远端宿主是否部署该逻辑须用户核对。


### 0.1.16 显式上游错误与子请求

同一参考提交的 `basispoints/stream.go` 将 `error` 视为终止事件并正常 emit，而不是传输断管。本插件据此处理 `error` / `response.error`（后者归一为 `error`）；真正的 `response.failed` 仍保留已收到的 response ID、usage 和上游错误码，不新增请求或改变现有宿主重试/换号策略。暂存工具在失败时丢弃并计入未释放数量，不执行、不写回放记录。

用户提供 v0.1.15 诊断 `3f59e74ea1b3f38fa14b4be322f1e8d8`（13:41:19）和子请求诊断 `34b0ef3c4b1b250e83a502f6a103e5f9`（13:50:51），均为 2026-09-26、账号 #58 / gpt-6-astra、BPS HTTP 200 / SSE，随后显式流内错误。两条都没有历史回放或输出工具，不能归咎于信封解析或回放缓存。旧版直接把事件转成通用错误，原文字段未保存，无法在升级后追溯。相同报错分支不证明两次上游根因相同。

子智能体不使用独立的插件协议或禁用开关，仍是独立模型请求。模型白名单拒绝在插件本地返回 HTTP 400 / `bps_model_not_allowed`，不发送 BPS；这与上述 HTTP 200 / SSE 路径不同。本轮没有新增允许模型、换账号、放宽权限或将失败标成功。新版保留上游语义错误字段供宿主既有策略处理，宿主若继续包装为 502，以插件新诊断原文为证据继续定位。

管理员错误字段按用户要求不脱敏，但不主动复制请求正文/头；字段有 64 KiB 上限，错误原文可能包含上游输入回显。面板使用 textContent 显示，不把错误字符串作为 HTML 执行。仅编译和归档校验，不代表父任务或子任务实机验收通过。


### 0.1.17 父调用加密声明与子消息边界（历史）

同一固定提交的 `tools.go::finishClientToolCall` 明确给明文 function_call 添加 `encrypted_function_args: []`，`translateDirectCatalogCall` 则保留对应原生调用的非 null 加密元数据。`agent_message_test.go` 覆盖 spawn_agent/send_message/followup_task、三处 SSE item、回放、direct 元数据保留和外层 code 声明不下传。这部分已存在于参考，但此前独立插件未移植；本轮补齐，不需要改宿主或全局 Subagent 偏好。

参考 `content.go::validateHistoryContent` 拒绝 agent_message 中的未知加密内容，测试明确不把“像明文”的字符串重新解释为明文。本插件只补这一子消息边界，不照搬全量 content 白名单去收窄已有图片/扩展字段透传。旧请求返回明确本地 400、专用错误码和输入字段位置；reasoning/compaction 中的合法加密状态不在该检查范围。

当前修复作用于此插件生成的新父调用；已生成/排队/保存在客户端的旧子消息不会被追溯修改。仅清空后台记录或重复提交失败请求不能获得新标记。由升级后的父任务重新委派，必要时新建父任务；检查父输出标记与子请求字段计数，再确认实际执行结果。编译/签名和源码对照不是客户端多代理验收。


### 0.1.18 撤销子消息本地拒绝

0.1.17 将参考 fork 的严格 content 白名单部分移植为加密子消息拒绝，阻止了旧会话在上游验证前继续。与此不同，原始两个参考项目对普通 input item 保留字段，并未以 agent_message/encrypted_content 名称进行拒绝。本版选择后者，保留 0.1.17 的新父调用明文元数据修复，移除本地 bps_agent_encrypted_content 分支。普通字段清理、回放、模型/账号限制保持不变。

升级后可在原任务重试；preserved 只证明消息保留到请求，仍以 Responses 发送阶段及 BPS 实际结果判断。真实上游解密失败不会被改成成功，不添加删除历史/换号/重复调用的自动恢复。无需改宿主或客户端配置。


### 0.1.19 静态复核

本轮继续只改独立插件，工作区 HEAD 仍为 4808f43a2。移除 0.1.17 的误拦截后，原有普通字段清理、工具恢复和账号隔离保持不变；图片回退及诊断不再深入不透明加密段。新增的是防止误改字段的边界，不是本地解密能力。

主动图片路由探测调用真实 Forward，会同时产生 probe 与 origin=image_route_probe 的路由记录，须按 route_diagnostic_id 核对。文本探测不使用这条路由历史作为自身结果。未知密文的真实 BPS 校验及桌面端继续旧任务的效果仍待用户验收；没有执行模型请求或部署。
