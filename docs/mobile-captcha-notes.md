# 安卓 1.97.1 与网页接口一致性核对（REA 复核）及移动端人机验证

本文是对 `docs/reverse-engineering.md`、`docs/web-generation-comparison.md` 的增量复核：用 REA（JADX headless 0.7.1，APK SHA-256 `01a454624aa3007a404b8555a4346bbca04f1f3c764645dfaaee22fc3d70e0fd`）在 2026-11-18 重新核对了生成链、验证码预检与设备证明链路。REA 本次产生的 Evidence：

| Evidence ID | 内容 |
| --- | --- |
| `ev_a384a2b25625b2976c47a6003a3c349db004966404f10348f718e42cf08b046b` | `GenParamsSpec` 全部 38 个字段与构造签名 |
| `ev_90767de12891bf14abc60d4bdf572eead886bc7b124002bd98b1823503e3f84f` | `CaptchaCheckService.checkCaptchaIsRequired`（`POST c/check`） |
| `ev_36c4f67562359fbcf5ad5853ff4fd33135b056e072c8a4fd1909fb048eb19112` | `RemoteCheckCaptchaResult` 仅含 `required` |
| `ev_b79c466fcd02d376606e789ab06c07451fdaa2c3679baf4286cb49bdd7122a3f` | `NonceService.fetchNonce`（`POST device_attestation/nonce`） |

REA 的 `search_android_classes` 全量搜索在该 APK 上仍触发 "JADX class inventory is duplicated or differs from the loaded class count"（加载 33,267 类后 inventory 校验失败）；本报告沿用既有做法：反编译导出定位精确类名 → REA `inspect_android_class`/`inspect_android_method` 定向核对。

## 一、安卓与网页接口一致性

**结论：两端是同一套业务 API 的两个客户端形态，核心字段语义一致，但请求形状不是同一个协议。** 生成端点、请求体字段集合、验证码预检端点均不同，不能把网页 Cookie 直接套进安卓请求形状（或反之）后宣称等价。

### 一致的部分

| 项 | 两端一致 |
| --- | --- |
| 会话鉴权 | `Authorization: Bearer <session JWT>` |
| 验证码预检查 | `POST c/check`，body `{"ctype":"generation"}`（安卓枚举唯一值 `m62.Generation`） |
| 生成语义 | `generation_type=TEXT`、`mv`、`prompt`（自定义=歌词）、`gpt_description_prompt`（简单=描述）、`tags`、`title`、`negative_tags`、`make_instrumental`、`metadata.create_mode=simple|custom`、`token`（验证码令牌） |
| 模型 | `billing/info/` 返回 `models[].external_key`，即 `mv` 取值 |
| `c/check` 返回 | `required` 布尔（安卓响应模型只有这一个字段） |

### 不一致的部分

| 项 | 安卓 1.97.1 | 网页（当前官网） |
| --- | --- | --- |
| Base URL | `https://studio-api-prod.suno.com/api/` | `https://suno.com/a/api/` |
| 生成提交 | `POST generate/v2/` | `POST generate/v2-web/` |
| 生成请求体 | `GenParamsSpec` 36 字段：含 `gpt_prompt`、`hybrid_rewrite`、`user_uploaded_video_id`、`creation_source`、`attached_lyrics/styles`、`model_version_name` 等 app 专属字段；构造调用默认 `stream=false`、`batch_size=2`、`gpt_prompt=null` | builder 恒构造 `transaction_uuid`、`token_provider`、`user_uploaded_images_b64`、`override_fields`、`metadata.web_client_pathname/create_session_token/user_tier/lyrics_model`；无恒定 `stream`/`batch_size` |
| 请求头 | `x-suno-client`、`x-suno-region`、`x-suno-timezone`（`pd4`） | `Device-Id`（`ajs_anonymous_id`）、`Browser-Token`（时间戳 Base64 wrapper）、`Accept-Language`、`Referring-*` |
| 验证码预检返回 | 仅 `required` | `required` + `captcha_version` |
| 验证码 provider | 内置 hCaptcha SDK（Stripe 打包版），无版本选择 | `captcha_version=2` → Cloudflare Turnstile；`=1` → hCaptcha |
| 歌曲查询 | `GET clip/{clip_id}/`（回退 `generate/clip/{id}`）、`POST feed/v3` | `POST feed/v3`、`GET clip/{clip_id}` |
| 队列接口 | 声明了 `queue/start`、`queue/inject`（`QueueService`） | 主链未见 |
| 设备证明 | `POST device_attestation/nonce`（`NonceService`）+ Google Play Integrity SDK | 未观察到 |

`GenParamsSpec.token` 是验证码令牌槽位：`pv7.j(v4i, String token, String, …)` 的第二个参数就是它，由调用方在 `required=true` 时传入（见下节）。两端都把验证码令牌放进生成请求体，但网页额外声明 `token_provider`（1=hCaptcha，2=Turnstile），安卓没有该字段。

## 二、手机端人机验证

**结论：安卓端存在完整的人机验证链路，且比网页多一条设备证明（Play Integrity）通道。验证码令牌只能由 App 内真实完成的 hCaptcha 挑战产生，sitekey 由服务端远端下发，没有硬编码，第三方脚本无法离线伪造。**

### 链路（静态证据）

1. 生成前调用 `CaptchaCheckService` → `POST c/check {"ctype":"generation"}`；响应模型 `RemoteCheckCaptchaResult` 只声明 `required`（REA ev_36c4f675）。
2. `required=true` 时（`pr7.c` / `vr7.c`）：调用 token 获取协程 `k62`，经 hCaptcha SDK 完成挑战；失败路径有 `"HCaptcha not initialized"` 与超时 `"captcha token request timed out"` 日志；拿到 token 后填入 `GenParamsSpec.token` 再提交 `generate/v2/`。
3. hCaptcha 以 **Stripe 打包版 SDK**（`com.stripe.hcaptcha`）内置，挑战用 `HCaptchaSize.INVISIBLE` + WebView（`tq4.c`），endpoint 配置为 `stripecdn.com`；sitekey 不是常量，来自远端配置（`RadarSessionWithHCaptcha(passiveCaptchaSiteKey=…, passiveCaptchaRqdata=…)`、`PassiveCaptchaParams(siteKey, rqData, tokenTimeoutSeconds)`）。
4. 第二条通道：`NonceService` → `POST device_attestation/nonce`（REA ev_b79c466f），配合内置的 Google Play Integrity SDK 与 `RadarOptions{hcaptcha_token, android_verification_object{android_verification_token, app_id}}`（`zuf`），把设备完整性证明与 hCaptcha token 一起上报（Radar 会话）。

### 对本项目（suno2api）的影响

- 安卓协议的 `c/check` 已在实现中；历史真机联调记录为 `required=false`（见 `docs/generation-notes.md`），日常生成一般不触发挑战。
- 若服务端返回 `required=true`：安卓协议需要**真实 hCaptcha token**（invisible WebView 挑战，sitekey/rqdata 由服务端下发），网页协议需要对应 `captcha_version` 的 Turnstile/hCaptcha token。两者都只能由用户在官方客户端/页面正常完成挑战后提供，程序不代答、不伪造——当前实现已按此设计（`--captcha-token` / 网关 `token` 透传，`required=true` 且无 token 时明确报错不提交）。
- 安卓端另有 Play Integrity 设备证明通道（nonce + integrity token）。静态证据不能证明普通文本生成必然要求它；但若服务端对移动客户端收紧，这是安卓协议独立于 hCaptcha 的额外门槛，第三方调用比网页协议更难满足。
- 因此对外暴露的网关 API 默认使用网页协议（与当前会话材料匹配），安卓协议保留为显式可选项，这一取舍与两端协议不可互换的核对结论一致。

### 尚未由本次静态分析证明

- `pv7.j` 完整指令体在全量导出中反编译失败（1151 条指令跳过），token 参数到字段的确证来自 `work/pv7-simple.java:713` 的构造调用与 Kotlin metadata 参数序（token 位于第 30 位）。
- 服务端在何种账号/风控状态下返回 `required=true`，以及 Play Integrity 何时被强制，只能在线上验证。
