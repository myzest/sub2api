# OpenAI Basis Points for Sub2API

独立的 Sub2API `.s2plugin` 插件，使用已有 OpenAI OAuth 账号，将选定账号的 Responses 请求转换为 BPS 协议。源码和生成文件都在此目录；不需要二改 Sub2API 主程序。

这是 **0.1.3 待实机验收版本**。本版依据两个参考仓库修正字段处理，移除插件自身对普通 input 类型的白名单拦截。BPS 属于参考项目使用的非公开兼容接口，字段转发不等于上游一定接受，也不证明模型“不降智”。逐项来源和边界见 [FIELD_MAPPING.md](FIELD_MAPPING.md)。

## 安装

适用宿主：本工作区对应 fork 的插件机制，Plugin Protocol / Transport API / UI Bridge v1，**HostService v2**。清单声明 `>=0.2.8 <0.3.0`，版本号本身不能替代这些接口要求；没有在你的服务器镜像上验收。

1. 将 `dist/trusted-publisher.yaml` 中公钥条目合并到服务器已有配置的 `plugins.trusted_publishers`，保留其他发布者，保持 `allow_unsigned: false`。首次添加公钥后重启 Sub2API。
2. 在插件管理中导入 `dist/oai-basispoints-0.1.3.s2plugin`。包内包含 Linux amd64、Linux arm64、macOS arm64 运行文件。
3. 启用本插件。如果已有 OpenAI OAuth 出站插件处于启用状态，先停用它：宿主的 `openai.oauth.outbound_transport.v1` 只有一个启用槽位，不能与 GPT Inspector 同时占用。
4. 将宿主此插件能力的灰度比例设为 **100%**，再通过本插件的账号白名单控制 BPS 路由。比例低于 100% 时，部分选定账号可能根本到不了插件。
5. 打开插件设置，刷新账号。在保持 BPS 路由关闭的情况下，选一个账号、模型和 effort，点击“保存设置并探测”。探测最多等待 120 秒，会消耗一次短文本请求的额度。
6. 探测成功后，选中需要走 BPS 的账号，再打开路由开关并保存。

默认 `route_enabled=false`、账号白名单为空。插件已启用但 BPS 路由关闭时，OAuth 请求经插件按原地址转发。

0.1.1 在后端、配置页和示例配置的默认允许模型中加入 `gpt-6-astra`。升级时先停用旧版，再导入新版并启用；沿用原发布者公钥。已保存的模型列表会保留，如需增加该模型，在「允许的模型」追加一行 `gpt-6-astra` 并保存即可，旧版也支持手动添加。

0.1.3 按参考代码保留消息、未知 input 类型及其嵌套字段；撤回顶层文本项改写为 message、压缩项删除 ID、尝试解析 item_reference 等缺少参考依据的特殊处理。`item_reference` 与无加密内容的 reasoning 按两仓库规则过滤，因此客户端仍需发送完整历史。工具结果保留结构化内容，普通 SSE 事件及终态扩展字段保留。尚未取得故障请求的实际 input.type，也未实机验收本版。

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
| 工具 | 支持客户端 function/custom、namespace 和串行调用；完整 schema / custom format 放入提示，function 参数用 JSON Schema 校验 |
| 原生信封 | 仅接受 `run_officejs` / `functions.run_officejs` 的严格 JSON `code`，解析 name/arguments/input 或 tool/args；不执行 JS、不从任意文本猜测工具、不自动修补 JSON |
| 工具回放 | 完整原生 item 存入宿主 KV，客户端收到随机句柄，下一轮恢复原生 item 与原始 call ID 的 output |
| 终态 | 保留 failed/incomplete、usage、incomplete_details 与扩展字段；缺少终态、非法工具、多工具、原生工具身份不一致均报错，不伪装 completed |
| 其他账号 | 按原 URL、Host、headers、body、proxy 转发，使用标准 Go HTTP/TLS；不会复现宿主定制 TLS 指纹 |
| Token / 错误 | Token 只在内存用于请求；HTTP 错误保留状态码及限流 headers，不透传原始错误体 |

向模型约定一次最多调用一个客户端工具；收到多个工具时明确报错，不擅自拆分。`custom.format` 会完整提供给模型，但插件不实现自定义 grammar 的本地解析器。工具仍由客户端按原来的权限与确认机制执行。

消息中的图片、文件及其他内容块原样保留，不做上传、下载、URL 重写、编码转换或 detail 修正。顶层 `text/input_text/output_text` 和 `compaction/compaction_summary/compaction_trigger` 保持原类型和位置，前置工具目录不会移走末尾压缩触发项。BPS 对这些输入的接受情况须由实机响应确认。

工具目录只提取 function/custom 和 namespace 下的对应叶子，其他工具声明与参考项目一样忽略；这不意味着原生搜索、计算机或 MCP 工具已经可用。不增加独立 `/responses/compact`、`/input_tokens`、后台任务或旧响应拉取能力。`previous_response_id`、`conversation`、`background:true` 明确报错。

顶层请求使用两仓库的 BPS 字段集合：缓存键原值传递，标量 metadata 保留并字符串化，已有 task/turn/iteration 不覆盖。`context_management` 数组原样传递，缺省使用两仓库相同的 compaction 阈值 200000。`max_output_tokens`、`temperature`、`top_p`、`text`、`truncation`、`include`、`parallel_tool_calls`、嵌套 `reasoning.summary` 及其他未映射顶层字段不发送；这些控制在本通道不生效。没有凭猜测扩大 BPS 请求 schema，也不会失败后移除字段自动重试。

由于插件收到的是宿主已规范化的出站请求，无法恢复被宿主改写的原始模型别名或原始 API Key ID。因此首版用账号白名单选择通道，不用 `-excel` / `-basispoints` 模型后缀识别。

## 回放与资源限制

工具句柄含 128 位随机值；KV key 绑定 sub2api account ID、模型、出站会话标识与首条用户消息指纹。缓存缺失、过期、参数被修改或账号变化时明确失败，不伪造原生调用。

宿主开启 `codex_fingerprint_mode=session/full` 时可能合并出站会话 ID。随机句柄能防止简单猜测，但现有插件接口**不能证明原始 API Key 级别的严格隔离**。不要将该机制描述为完整租户授权边界。

默认工具回放保留 24 小时，可设置 5 分钟至 7 天。KV 会保存原生工具参数及客户端调用，可能包含业务内容；不保存 OAuth token 或工具执行结果。插件自身也校验过期时间，实际持久化能力取决于宿主 KV / Redis 配置。

单请求最多 8 MiB，响应最多 32 MiB，单条 SSE 最多 4 MiB，单条回放记录最多 256 KiB；超限报错。配置超时默认 600 秒，客户端取消会传递至上游。插件不会自动重试已发送的 BPS 请求。

## 你可以这样验收

1. 原样导入现有 sub2api OAuth 账号，执行单账号探测，记录 HTTP 状态、返回模型和 usage。
2. 使用专用分组发一条文本请求，确认 Responses 流能结束。
3. 让 Codex 执行一次简单工具（例如 `pwd`），再根据结果回答，确认工具调用和第二轮回放都成功。
4. 测试取消、账号代理，以及路由关闭后的普通转发。
5. 对同一个账号、模型、题目和 effort，在两个通道各跑多轮，比较成功率、正确率、耗时和 reasoning tokens。不要仅凭回复长度或模型自述判断质量。

401/403 重点排查 token / account ID / 访问条件；400/422 排查模型、effort 与输入兼容；429 是限流或额度信号。真实部署遇到错误时保留状态码、Response ID、插件错误码即可，无需提供 token。

## 本地构建

运行依赖 Go 1.26+。UI 是静态资源；安装包运行不需要 Node.js、Python、Excel 或浏览器。

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
