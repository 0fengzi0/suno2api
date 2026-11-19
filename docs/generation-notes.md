# 歌曲生成实现与验证

生成客户端在 `internal/generate`，使用 Go 标准库。**默认安卓协议**地址为 `https://studio-api-prod.suno.com/api/`（真机验证可用）；网页协议 `https://suno.com/a/api/` 作为显式备用。每个 profile 的主机和路径前缀受明确限制，不会将 Bearer 转发到其他域名或跟随鉴权请求的重定向。

## 当前成功结果

2026-11-17 19:52（Asia/Shanghai），按单独授权仅提交一次网页 v6-mini 请求；正规 `c/check` 返回 `required=false`，实际请求 `mv=chirp-goose`，不带安卓的 `stream`、`batch_size`。服务请求 ID 为 `efcc505e-a206-476b-ab50-5eba05a9b5aa`，剩余额度从 20 变为 10。

两首目标 v6-mini 歌曲 `67d3ee70-a875-4de2-8e57-d1d34a6b57ad`、`065338e2-e0bf-49f3-ab82-e573b7c3bfb6` 随后均达到 `complete` 并提供媒体。服务另返回两首额外模型歌曲；代码只等待请求模型。实际请求正文和响应 ID 的脱敏记录在 `work/live-generation-web.json`，不含鉴权头、Cookie、播放许可或签名媒体 URL。本地 guard 防止脚本再次提交。

已完成歌曲的下载单独验证；前两次尝试在 CDN 传输阶段超过两分钟，未发布残缺文件。原会话随后过期，重新获取播放许可需要用户更新同一 token 文件。生成完成和文件保存是两项不同证据。

## 官方网页协议

来源为实际 `suno.com/create` 页面的官方 JavaScript，逐条证据见 `web-generation-comparison.md`。默认网页模式使用 `/api/generate/v2-web/`（完整地址 `https://suno.com/a/api/generate/v2-web/`），正规检查 `/api/c/check`，状态查询 `/api/feed/v3`。

只从用户原始 Cookie 提取会话 `__session` 和实际 `ajs_anonymous_id`；后者按官方代码 URL 解码、去引号并过滤非字母数字和连字符后放入 `Device-Id`。未提供现有设备 ID 时明确报错，不在当前用户身份限制下自动制造新身份。`Browser-Token` 为官方普通格式 `JSON{token:Base64(JSON{timestamp:当前毫秒})}`，不是服务器签名或人机验证 token。

每次提交生成 UUIDv4 `transaction_uuid`，同一客户端实例复用本地创建会话 UUID。metadata 包含实际页面路径、模式、关闭音量归一化标志，简单模式补 `lyrics_model=default`；账号 tier 从 billing 的当前 plan.id 或 Free plans fallback 获取，缺失就省略。网页模式不凭安卓协议推测额外必需字段。真实用户验证码 token 仅在用户已完成验证时使用，provider 依官方规则为 version===2 ? 2 : 1。

## 安装包证据

目标为用户提供的 Suno Android 1.97.1（478），APK SHA-256 为 `01a454624aa3007a404b8555a4346bbca04f1f3c764645dfaaee22fc3d70e0fd`。REA 包声明证据为 `ev_9f57cad736613adfa41f6e4dd20187681f7705f80e56b721b7edaa5f0c107340`；完整调查和各静态来源见 `reverse-engineering.md`。以下是静态观察，不能单独证明线上服务仍接受所有参数。

- `GenerateService.generateApiRunGenerationV2`：`POST generate/v2/`，Body 为 `GenParamsSpec`，返回 `GenerationRequestSchema`。
- `GenerateService.generateApiGetClip`：`GET generate/clip/{clip_id}`，返回 `RemoteClip`，供任务轮询。
- `GenParamsSpec` 的 Kotlin serialization 明确规定 `prompt`、`mv`、`make_instrumental`、`generation_type`；文本类型序列化为 `TEXT`。APK 默认 `stream=false`、`batch_size=2`。
- `defpackage.pv7.j` 的实际构造调用将描述映射为 `gpt_description_prompt`，自定义歌词映射为 `prompt`，风格映射为 `tags`，标题映射为 `title`。`GenMetadataSpec.CreateMode` 将模式序列化为 `simple` 或 `custom`。`task` 可空，没有根据字符串猜造任务常量。
- `BillingService` 的 `GET billing/info/` 返回 `SubscriptionInfoResponse.models`；`RemoteModel` 提供 `external_key`、`can_use`、`is_default_model` 和 `is_default_free_model`。
- APK 的 `SunoAuthHeaderInterceptor` 使用 `Authorization: Bearer <session token>`。
- `CaptchaCheckService` 的 `POST c/check` 请求为 `{"ctype":"generation"}`，返回必需字段 `required: bool`。真实生成调用方会先检查，要求验证时再取得挑战 token，并将它放入 `GenParamsSpec.token`。

## 客户端行为

`NewClient(Config)` 创建客户端，`Generate(ctx, Request)` 仅提交一次，`Get(ctx, ids)` 查询歌曲，`Wait(ctx, ids)` 等待歌曲 `complete` 且提供音频媒体。`Response.IDs()` 提取全部歌曲 ID，`RequestedIDs()` 优先提取与实际请求模型一致的歌曲。`Billing(ctx)` 仅投影模型与额度，避免在调用输出中夹带账号个人资料。

简单生成使用 `Prompt` 描述；自定义生成使用 `Custom`、`Lyrics`、`Style`、`Title`。纯音乐设置 `Instrumental=true`。模式只由 `Custom` 或实际歌词触发，`Style`/`Title` 不会把简单描述误判成自定义歌词；自定义纯音乐时 `prompt` 固定为空、只发送 `tags`，不会把音乐描述送去演唱。网关和网页表单都按这条规则归一化。安卓 `BatchSize` 可设 1 或 2；网页 profile 按当前官方 builder 省略该字段。按用户指定，默认模型固定为 v6-mini，对应 `chirp-goose`，支持该别名，不会自动改选付费模型。

线上验证发现旧 `generate/clip/{id}` 路径对当前会话返回 403，而已证实 `GET clip/{id}/` 返回正确状态，因此查询使用现行 `clip/{id}/`，404 时才回退旧静态路径。APK 的只读 `POST feed/v3` 精确 ID 查询也验证为 HTTP 200，状态与公开 clip 查询一致。

鉴权支持 `Config.Token`、`Config.TokenFile`、`SUNO_TOKEN` 或 `SUNO_TOKEN_FILE`。文件可保存单个 Bearer、JWT，或用户自己提供的 Suno Cookie；Cookie 只提取 `__session`（兼容后缀名称），网页模式另取 `ajs_anonymous_id`，不会向 API 发送整份 Cookie。只有 JWT 的网页调用还须显式提供现有 DeviceID。每次请求重新读取文件，便于刷新短期会话。会话 JWT 带 `exp` 且已过期时返回明确错误；此检查不验证 JWT 签名。

代理可通过 `Config.ProxyURL`、`SUNO_PROXY` 或标准 `HTTP_PROXY` / `HTTPS_PROXY` 配置，支持 `ALL_PROXY` 回退。Windows 系统代理由上层公共网络客户端读取并注入。不会修改操作系统网络设置。

生成前使用 `CheckCaptcha(ctx)` 调用正规检查；缺失或不完整的检查结果会阻止提交。`required=true` 且没有用户手工完成验证取得的 token 时返回 `ErrCaptchaRequired`，不会发生成 POST。任一生成链路响应（含 `c/check` 自身）带 `CF-Mitigated: challenge` 头时同样归类为 `ErrCaptchaRequired`——这是 Cloudflare 对请求形状或出口 IP 的浏览器验证要求；不带该头的普通 403 仍按原错误分类处理，不冒充人机验证。`CheckCaptcha` 的结果可通过 `Config.OnCaptchaCheck` 回调上报给调用方。生成 POST 不自动重试，以免重复扣点。401、402、429 分别返回可识别错误。轮询默认每 15 秒一次，429 和服务器暂时错误尊重 `Retry-After`，并受调用 context 和默认 10 分钟上限限制。超时后保留原 ID 以便恢复查询，不能将超时当成服务端终态。失败歌曲返回 `GenerationError` 和已取得的歌曲状态。

## 验证范围

- `go test ./internal/generate` 与 `go vet ./internal/generate` 通过。
- 本地 HTTP 测试覆盖：实际路径和请求体、v6-mini 默认模型及别名、提交后轮询、流式 URL 不提前算完成、生成 POST 不重复、token 错误脱敏、禁止鉴权重定向、失败状态、超时取消、凭据文件更新、错误分类和目的地址限制。
- 使用用户本次明确提供的会话和本机现有代理，真实 `GET billing/info/` 返回成功，证明当时会话可用于该接口。服务器确认账号可用模型为 `chirp-goose`；用户随后明确指定免费账号使用 v6-mini，客户端按此固定默认值。
- 初次安卓验证仅发起一次真实生成 POST，请求代码经当时 `billing/info/` 可用默认模型选择路径使用 `mv=chirp-goose`、`batch_size=1`、纯音乐。没有保存原始 HTTP 请求包，所以该 mv 依据是客户端执行路径和已核验的账号模型响应，不能当成原始抓包证据。
- 服务端返回两首，实际 `model_name` 分别为 `chirp-hawk` 与 `chirp-goose`。截至本次复核，两首均为 `submitted`、尚无媒体。公开 clip 的无缓存查询和 APK feed/v3 查询一致，不能声称生成已完成。ID 已保存，未再次生成；后续应优先跟踪所请求的 v6-mini 那首。
- 本次原始生成早于正规 `c/check` preflight 补充，当时只提交了基本生成参数。客户端现已补齐该检查；是否与原任务持续排队有关尚无证据，不能归因。随后执行了一次下述修正版独立验证，没有自动重试原任务。
- 在单独授权的一次修正版验证中，Go 和 PowerShell 均确认正规 `c/check` 为 HTTP 200、`required=false`。客户端随后显式请求 v6-mini，已在发往官方 API 的请求体中记录 `mv=chirp-goose`、`batch_size=1` 与纯音乐参数，记录时删除验证 token，不记录鉴权头。一次生成使 `total_credits_left` 从 30 变为 20，证明服务端已受理并扣除额度；仍不能据此声称音频已完成。
- 修正版生成请求 ID 为 `03210acd-ecb1-43b5-94a5-08e22c0bd273`；主要 v6-mini 歌曲 ID 为 `9379be2d-dcf6-4773-8f5e-1270f48cab21`。服务同样返回额外 v6 歌曲 `dc6c054c-a9a2-4ae7-9e82-e76892bd64e8`。本地 guard 阻止再次运行该生成验证，后续仅按 ID 查询。
- 先前 `c/check` 的远端关闭现象没有复现，两个客户端的正常同协议请求均成功，因此只能记为当时的传输层失败，不能归因为 WAF 或接口协议错误。旧 `generate/requests/` 使用实际请求 ID 仍返回 HTTP 410；当前状态以可用的公开 clip 与 APK feed/v3 查询为准。
- 已补回归检查：配置中的下载目录即使尚不存在也创建为目录，显式输出文件语义不变；轮询识别已完成的 `m4a-opus` 等裸类型播放媒体；HTTP 生成若已收到请求 ID 或歌曲而后响应校验失败，保留 `submitted`、原响应和 ID，提醒不要自动重提。
- 网页协议回归覆盖：精确 API host/prefix、正规检查路径与 body、Device-Id 仅来自当前身份、不会传整份 Cookie、双层 Browser-Token 时间戳、metadata 与 UUIDv4 形状、同实例会话固定与每次事务唯一、缺失 tier 不伪造、验证码 provider fallback、web feed 字段与裸媒体格式。
- Windows 本机关闭 CGO，`-race` 检查未执行。

Clerk 刷新接口虽然在 APK 中可见，但本实现不复制整套登录 SDK；若短期凭据过期，应从用户自己的 Suno 会话刷新后更新 token 文件。
