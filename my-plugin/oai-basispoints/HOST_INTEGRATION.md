# ranxi2001/sub2api 对照与路由边界（0.1.15）

检查日期：2026-09-26。参考仓库：<https://github.com/ranxi2001/sub2api>，production 固定到 `f671a8d30c34706d8526accadf6a6ad5f40f855e`（2026-09-26 11:47:46 +08:00）。这里只报告该提交源码，不推定远端服务已部署它。当前工作区 origin 是 myzest/sub2api，本轮开始 HEAD 为 f62cf9e84，未包含下面整套原生 Excel 适配。本轮只修改独立插件，没有合并或部署宿主。

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

1. 先确认入口；插件路径升级后应产生 v0.1.15 新记录。原生路径没有新插件记录可能正常，勿用旧记录定位新请求。
2. 新任务请求读本地文件，functions.exec 的解析记录应出现 CUSTOM 原文；schema 声明 code:string 的函数可出现 FUNCTION_CODE 原文＋元数据 JSON。普通 function、旧会话仍兼容 JSON。
3. 继续完整会话核对回放。文件读取、浏览器、Subagent 和权限仍由客户端执行；升级不修改客户端权限。
4. 若再出错，核对终态与诊断 ID，HTTP 200 不等于成功。只有完整响应的信封转换失败改为明确 failed；网络中断、取消或无可靠终态仍可能断流。不同宿主版本的错误脱敏/透传规则需用户实测。

### 失败终态与宿主换号边界

另检查了该 fork 的 openai_gateway_passthrough.go::openAIStreamFailedEventShouldFailover：尚未输出语义内容时，未分类的 response.failed 默认可触发换号。插件因此用宿主已有的 invalid_request_error 不可重试分类承载确定的信封协议错误，保留专用 bps_tool_envelope_invalid 码及明确的 BPS 输出错误说明，不归咎客户端输入。宿主可能在对外失败事件中删除 output/usage 等字段，插件端仍保留观察到的终态和用量；远端宿主是否部署该逻辑须用户核对。
