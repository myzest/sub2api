# 0.1.21：Codex WebSocket 与插件保活

## 范围与源码依据

只更新 `my-plugin/oai-basispoints`。不修改宿主源码、另一个插件、账号、线上配置或本机 Codex 配置，不自动部署或发起真实请求。当前本地宿主 HEAD 为 `1f1b9966f`，用户确认与线上对应；其他版本必须具备下述桥接机制，不能仅凭宿主版本号推定支持。

```text
Codex --WebSocket--> sub2api 现有 /responses 或 /v1/responses 入口
      --宿主内部 HTTP Bridge--> BPS 插件 --HTTP/SSE--> BPS
```

核对的宿主文件（均未修改）：

- `backend/internal/server/routes/gateway.go`：现有 Responses WebSocket GET/Upgrade 入口。
- `backend/internal/service/plugin_manager.go`：`ShouldRouteOpenAIOAuth` 依据 OpenAI OAuth 插件绑定及账号灰度判断。
- `backend/internal/service/openai_ws_forwarder_ingress.go`：命中插件绑定时强制 HTTP Bridge，不要求 BPS 支持原生 WebSocket。
- `backend/internal/service/openai_plugin_transport.go`：桥接继续调用插件出站传输。
- `backend/internal/service/openai_ws_http_bridge.go`：首个语义输出前，`keepalive` 单独写入客户端，不提交暂存元数据，也不因心跳将 `wroteDownstream` 设为 true。
- `backend/internal/service/openai_ws_http_bridge_test.go`：已有 `TestProxyOpenAIWSHTTPBridgeTurnStagesMetadataAndRelaysKeepaliveBeforeCapacityFailover` 回归源码；本轮未运行。

普通 HTTP 的首输出缓存路径仍会缓存 metadata，包括 keepalive。关闭首输出超时或只调整注释心跳间隔不能代替 WebSocket 链路。

## 插件行为

1. 收到 BPS SSE HTTP 响应、开始消费后，按独立 15 秒周期发送 `event: keepalive` 与 `data: {"type":"keepalive"}`。不等待上游 response ID。
2. 工具参数增量、已暂存的后续项、真实 created/in_progress 事件以及成功写出的元数据，都不能重置保活计时。单看插件成功写出并不能证明宿主已经放行，所以不能仅按插件下游活动决定是否保活。
3. 心跳不含 response ID、output、usage 或 sequence_number。宿主可能把心跳先于之前缓存的 created 事件送到客户端；给心跳添加 Responses 序号会造成不必要的顺序歧义。真实 Responses 事件继续连续编号，只有成功写出才推进序号。
4. 所有写出仍由同一消费循环串行进行；保活不提前释放未校验的工具，也不使用额外上游请求。取消、结束或写出失败即停止。独立周期不等于硬实时保证：同步转换、写入背压或宿主阻塞仍需实际诊断。
5. 非流式 JSON 聚合不发送心跳；Images 请求、模型、effort、账号和重试策略保持原有逻辑。发送 BPS 响应头前的身份获取、图片上传、连接等待不在此 SSE 保活阶段内。

## 安装与客户端设置

1. 停用旧插件，导入并启用 `oai-basispoints-0.1.21.s2plugin`，沿用原发布者公钥和插件配置。保留已有 BPS 路由、账号白名单及模型设置。宿主插件灰度需覆盖测试账号，原有建议是 100% 灰度加插件内部账号白名单。
2. 在实际使用 Codex 的电脑打开用户 `config.toml`，当前 Mac 为 `/Users/zest/.codex/config.toml`；Windows 默认是 `%USERPROFILE%\.codex\config.toml`。若设置 CODEX_HOME，以该目录为准。配置管理器管理 provider 时，应同时确认其不会覆盖手动编辑。
3. 找到顶层 `model_provider` 对应的已有 `[model_providers.<名称>]`，保留 `wire_api = "responses"` 并添加 `supports_websockets = true`。示例见 `codex-websocket.example.toml`。不要重复添加同名表，不要将字段放进 `http_headers` 子表。保留原 base_url、鉴权和图片功能头，不手动把地址换成 BPS。
4. 完全退出并重开该电脑的 Codex，以新请求验证；本包不自动编辑客户端配置。官方字段依据：<https://learn.chatgpt.com/codex/config-reference>。配置被读取不等于实际已建立 WebSocket。
5. 若入口前有 Nginx、网关或其他反向代理，确认 Responses 路径支持 WebSocket Upgrade 和足够的连接寿命；直连现有宿主端口无需新增中继服务或端口。不能仅凭本地源码判断线上反代配置。

宿主没有本方案必须另开的 BPS HTTP Bridge 开关：命中上述插件绑定后由代码自动选择。不要为此盲目更改所有账号的通用 WebSocket 模式。

## 诊断与验收（由用户执行）

- 实际握手：客户端/入口记录显示 WebSocket 握手成功，而不是仍然发送普通 POST/SSE。`openai.websocket_ingress_started` 在 Accept 之前，只证明进入 handler；结合握手结果和 `ingress_ws_http_bridge_start` 判断桥接是否启动。
- 对应插件记录：确认 v0.1.21、实例和诊断 ID。插件 Responses HTTP 200 是插件与 BPS 的上游状态，不能由此推定客户端传输协议。
- 长等待：用原有模型和 effort 重试先前长工具参数/长文本任务，检查 keepalive 次数及插件→宿主写出时间，再结合客户端事件或请求结果确认心跳是否到达和任务是否真正完成。单账号主动探测使用模拟/聚合链路，不能替代客户端 WebSocket 验收。
- 多轮与终止：核对工具结果回放、连续轮次、Subagent、用户取消与断线重连。每项都需真实客户端验收，当前包没有证明任意旧会话或跨账号不透明状态可恢复。
- 回归边界：仍须保留真实 error/failed/incomplete。心跳不会延长 timeout_seconds、消除上游容量错误、修复密文或网络解码错误。只解决当前有明确源码依据的保活阻断环节。

新增 `stream_delivery` 仅保存数值和时间：已处理上游事件数、首次/最后上游事件时间、累计缓存后续事件数、未直接转发的原生工具事件数、结束时仍待处理缓存数、成功写入宿主的 JSON 事件数、首次/最后成功写出时间及最大已观测写出静默。时间戳为 Unix 毫秒；间隔使用 Go 单调时间，包含首次等待与结束前静默。成功写出计数包含心跳但不包含 `[DONE]`；缓存和工具重建导致输入输出数量不一定相等。写出失败不计成功。没有记录正文、工具参数、图片或凭据。

这些数据不是宿主刷新、客户端收包或模型质量的证明。普通 HTTP/SSE 首输出前仍可能缓存 keepalive；若客户端自动降级到 HTTP，本方案不能保证继续具备 WS Bridge 的心跳放行效果。

## 回滚

保留原配置备份及旧插件包。需要时恢复原 provider 的 supports_websockets 设置并重开客户端；若需撤回插件行为，停用本版后导入原版本。其他模型、鉴权、图片头和发布者公钥不必更换。恢复 HTTP 会重新具有原首输出缓存限制，不代表保活问题已解决。
