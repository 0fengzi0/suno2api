# Android 1.97.1 请求头复核

本次针对生成持续 `submitted` 的现象，仅复核用户指定 APK 中的正常网络主链；未创建新歌，未把请求头差异认定为排队原因。主 APK SHA-256 是 `01a454624aa3007a404b8555a4346bbca04f1f3c764645dfaaee22fc3d70e0fd`。

## 正常主 API 的确定差异

`defpackage.w74` 的 provider case 36（源码导出 626–660 行）构造普通 API 的 OkHttpClient，注册 `gm1(pd4)`、`SunoAnalyticsIdHeaderInterceptor`、`ApiMetadataHeaderInterceptor`、`SunoAuthHeaderInterceptor` 等。case 179（1414 行）把同一个 Retrofit `n84.X` 交给 `GenerateService`。因此下面这些不是只存在于 SDK 的无关字符串，而是在生成所用主客户端上注册的实际构造逻辑。

| Header | APK 构造来源 | 静态语义 |
| --- | --- | --- |
| `Authorization` | `SunoAuthHeaderInterceptor:40` | 当前正常会话 JWT，前缀 `Bearer ` |
| `x-suno-client` | `gm1` 默认分支调用 `pd4.b:49–53` | Android 应用身份及版本；本样本为 `android 1.97.1 (478)` |
| `x-suno-region` | `pd4.b:53` | 原样取 `Locale.getDefault().getCountry()` |
| `x-suno-timezone` | `pd4.b:53` | 原样取 `TimeZone.getDefault().getID()` |
| `anonymous-id` | `SunoAnalyticsIdHeaderInterceptor:31` | `q99.a()` 返回的本地安装标识 |
| `session-id` | 同上 32–49 行 | 可空；取得响应 `session-id` 后保存在进程状态供后续请求使用 |
| `Content-Type` | `ApiMetadataHeaderInterceptor:21` | `application/json` |
| `Accept-Language` | 同上 17–22 行 | `Locale.getDefault().toLanguageTag()` |

当前 Go 生成客户端已发送 Bearer、JSON、Accept 及自己的 User-Agent；此前没有实现上述三项 `x-suno-*` 普通元数据、语言和分析标识。这是可复核的请求形状差异，不能仅靠静态代码证明服务端将其当作生成前置条件。

`x-suno-region` **没有空值自动改成 US 的分支**。Java Locale 可以没有 country；照 APK 行为应保留真实结果，不能把未知地区写成美国。本次用户上下文的真实时区是 `Asia/Shanghai`；Android 实际头仍由其设备时区决定，而非安装包硬编码。

`anonymous-id` 的 header 构造未验证 UUID 或固定长度。`q99.a()` 返回内部 `Id` 的字符串，`Id.constructor-impl` 只检查非 null 并原样返回，也无 36 字符限制。此次没有进一步恢复本地标识初始化的所有存储分支，所以不能把 UUID 36 字符格式提升为已证实的强制协议规则；更不能在没有映射证据时把网页 Cookie 中的 `suno_device_id` 改名成这个 Android 标识。若使用用户实际已有标识，应记录来源、原样保留，并做 HTTP 头基本合法性检查。

`x-suno-client` 在 APK 中确实表示 Android 应用版本。Go 工具应公开自己的实现身份，或使用用户明确配置且来源可复核的正常客户端元数据；不能因为这个字符串存在就假称运行在 Android。版本、地区、时区字段均不是新增账号权限。

## 条件性的 Android 设备证明

主客户端还注册 `goe` 拦截器。它仅在 `Feature.PlayIntegrityAttestation` 开启且当前路径匹配动态 `PlayIntegrityProtectedPaths` 时，尝试正常设备证明。开关关闭、路径不匹配时不附加证明字段；异常分支会继续原请求。静态安装包无法证明本次用户会话实时下发的开关或保护路径。

REA 定向恢复 `goe.a`，Evidence ID 为 `ev_950fd71318b8b629c72a83f83f69f09cb9d6ebcbaf7920fa9ae9ea1e48a7c60a`，完整记录在 `work/rea-attestation-interceptor.json`。它成功取得真实证明后设置 `Suno-AppAttest-Platform`、`Suno-AppAttest-Nonce`、`Suno-AppAttest-Integrity-Token`。这些属于条件性的正常 Android 证明，不是可以复制固定值或编造的普通 header。当前 Go 不伪造它们，也未把它们认定为当前 `submitted` 的原因。

主链中另一个 Stripe Radar 拦截器仅匹配 `/sign_ins`、`/sign_ups`、`/clerk/auth/birthday`，因此它的 `X-Stripe-Radar-Session` 不是生成接口必发头。没有在这条已核对的生成主链中发现固定 `client-token`。

## 可验证的下一步

补齐真实来源的普通元数据并以现有读接口确认网络层实际发出的头；保持已提交歌曲 ID 继续查询。正常账号读取成功只证明该请求被接受，不能证明新生成排队问题已经解决。若以后需要进一步确认生成差异，应比较用户正常客户端的合法请求记录，不能从 `submitted` 反推某个头必需，也不能用不断新建歌曲来试错。

本轮精选证据在 `docs/evidence/request-headers.json`，只包含静态源码和校验值，不含用户 Cookie、JWT、设备证明值或新的生成请求。
