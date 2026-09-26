# Codex Inspector 实现与适配记录

依据用户提供的 2026-09-24-codex-inspector.public.md 实现，当前工作区宿主版本为 Sub2API 0.2.8。参考文档中的 agent 命令、自动提交、发布、修改其他技能等内容不作为用户操作授权。

## 1. 先核对当前宿主

- 本仓库已有 my-plugin/gpt-inspector 和 my-plugin/oai-basispoints，新插件沿用布局，独立于两者。
- 标识为 local.codex-inspector，module 为 local.sub2api/codex-inspector；不是 Codex marketplace 插件。
- 公开 SDK 提供 TransportPlugin v1、UI Bridge v1、HostService v2。五个契约文件逐字复制，SHA-256 见 UPSTREAM.json，不依赖宿主私有 Go 包。
- 文档依赖的 codex-state-keeper 与配套设计文档未提供，因此按已给出的协议和功能要求实现基础设施，没有伪造缺失源码。
- 使用本机 Go 1.26.4 及已有插件的固定依赖；没有为文档中的 Go 1.27 升级工具链或依赖。

## 2. 时区改写路径

1. 账号覆盖优先，随后选择自定义、账号稳定随机、出口 IP 三种策略之一。
2. GeoIP 走账号代理；代理缓存键为 SHA-256，直连使用 direct。KV 读取上限 500ms，查询默认上限 3s，失败负缓存 10 分钟。
3. 先 JSON 解码，再检查 user 消息的 input_text 是否以 environment_context 开头，因此宿主 WebSocket→HTTP 桥接的 Unicode 转义也能处理。
4. 只替换匹配环境块内首个 timezone 和正常 current_date 标签；自闭合不可用日期不改。日期按目标 IANA 时区计算。
5. 保留未知字段与数字原文；没有变化时返回原字节。有变化后重算 Content-Length，并清除旧 Content-Length / Transfer-Encoding 头。
6. 可选增强失败时继续发送原体或使用配置的回退时区。真实上游网络错误如实报告，不伪造成功；已可能发送的请求禁止宿主无条件重放。

整包请求体上限 64 MiB；响应按 32 KiB 分块流式转发。与当前宿主实际帧协议一致，必须收到 body_end；提前 EOF 按不完整请求拒绝，避免转发截断内容。

## 3. 修正检测任务触发方式

当前宿主 backend/internal/service/plugin_manager.go 的保存路径是先 ApplyConfig、再加密与持久化；配置回滚、启动及临时测试进程也会 ApplyConfig。直接照参考计划从 ApplyConfig 启动检测，会出现配置尚未保存就消费额度、临时进程提前领取任务的风险。

1. ApplyConfig 只更新配置，不启动检测、不写执行标记。
2. 管理员在插件 UI 确认消耗额度后，依次 config.save → 核对已保存任务 → config.test。
3. task_id 绑定当前进程随机 instance，旧进程、未启用时创建的临时进程和重启后的旧命令不能领取任务。
4. 校验创建时间 ±30 分钟，检查执行标记，用进程内互斥防止同时提交两个批次。
5. Runner 取得租约后再次检查幂等；先保存初始任务，再写执行标记，任一步失败均不发送探测。
6. 独立续租；丢失租约或写结果失败立即取消。每账号串行、跨账号并发，401/403/429 停止该账号剩余项，不自动重试。

宿主 KV 没有 CAS/SET-NX，租约只能尽力协调，不能声称多副本严格恰好一次。UI Bridge v1 的保存和执行不是原子事务，应在一个配置窗口操作；多副本部署还需要稳定路由至同一插件实例。

## 4. 可追溯的指纹算法

- ModelTrace revision：55a2e4a55170423b484d701e9a82ab62b268c811。
- bank SHA-256：1c2cb74d372f9f0f30d0dabbb7b7a838660d2f769a88d0c8489e4c662e088c21。
- 原库和完整打分算法已移植；8 个 GPT 候选各取单回答、三回答两组，共 16 组真实参考回答，与原 JS 输出以 1e-9 容差对拍。
- 来源、MIT 许可和可重建 golden 工具随源码保留；许可证与 provenance 也随安装包分发。
- 概率只表示候选库内的统计归因，不能单独证明真实身份、服务降级或智力变化；时区改写也不保证改善模型能力。

## 5. 验证和交付边界

验证覆盖 Go 单元/并发测试、真实 go-plugin 子进程与反向 HostService broker、离线假上游、受限 iframe UI、包签名与文件哈希。具体执行结果见 VALIDATION.md。

本次不调用真实 OAuth 账号，不改变生产账号、宿主配置或现有插件启用状态，不提交或推送 Git，不创建远端发布。真实部署前仍须按 README 配置发布公钥、处理单 transport 路由冲突，并由管理员主动启动少量探测验收。
