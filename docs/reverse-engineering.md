# Suno Android 1.97.1 协议分析

本报告分析用户提供的 XAPK。以下“已观察”指安装包字节、REA 或反编译结果中的静态事实，不等于已在 Suno 线上成功执行。线上联调结果另见项目使用说明。

## 样本与分析边界

| 项目 | 值 |
| --- | --- |
| 包名 | `com.suno.android` |
| 版本 | `1.97.1`，version code `478` |
| XAPK SHA-256 | `b3b592814ffd3a378c772b637b3d9f5409f1c0168c7a59ff8faab73e6e3aff97` |
| 主 APK SHA-256 | `01a454624aa3007a404b8555a4346bbca04f1f3c764645dfaaee22fc3d70e0fd` |
| 安装包结构 | 主 APK、`config.arm64_v8a.apk`、`config.mdpi.apk`、清单、图标 |
| 主 APK 代码 | `classes.dex` 至 `classes6.dex`，Java/Kotlin/Android 客户端 |

已使用 REA 6.4.0 的包清单、安全提取、Android 声明、单类成员、方法和引用分析。初始 MCP 缺少 `REA_JADX_MCP_JAR`；找到用户已有的 `E:\tools\jadx-headless-mcp` 后，通过进程局部环境变量运行现有 REA CLI 和 JADX，未修改全局配置或安装新引擎。完整 JADX 导出有 149 个方法反编译错误，因此只对实际核对的类和字段下结论。

## 已观察的服务器协议

生产 API 基地址是 `https://studio-api-prod.suno.com/api/`。证据是 APK `defpackage.gmn` 的环境配置构造，而非依据历史项目猜测。下面的路径均相对于该地址。

| 功能 | 方法与路径 | 反编译类 |
| --- | --- | --- |
| 生成歌曲 | `POST generate/v2/` | `remote.generate.GenerateService` |
| 查询单曲生成结果 | `GET generate/clip/{clip_id}` | `GenerateService` |
| 查询生成请求 | `GET generate/requests/?ids=...` | `GenerateService` |
| 生成歌词 | `POST generate/lyrics/` | `GenerateService` |
| 查询歌词结果 | `GET generate/lyrics/{request_id}` | `GenerateService` |
| 读取歌曲元数据 | `GET clip/{clip_id}/` | `remote.clips.ClipsService` |
| 解析分享短码 | `GET share/code/{shortcode}` | `remote.share.ShareLinkService` |
| 下载授权 | `POST download/authorize` | `remote.download.DownloadAuthorizationService` |
| 获取可下载文件地址 | `GET download/clip/{clip_id}?format=...` | `DownloadAuthorizationService` |
| 账号订阅及可用模型 | `GET billing/info/` | `remote.billing.BillingService` |

`ShareLinkService.unwrapShortcode` 的结果 `UnwrappedShortcodeSchema` 为 `{success, content_id, content_type}`，三个字段都在 Kotlin 序列化描述符中声明。分享短码和歌曲 UUID 不是同一种标识。

## 生成请求及结果

`GenParamsSpec$$serializer` 明确声明四个必需字段：

```json
{
  "prompt": "歌词或空字符串",
  "mv": "账号可用模型的 external_key",
  "make_instrumental": false,
  "generation_type": "TEXT"
}
```

这只是从序列化描述符恢复的最小数据结构；服务端可能还有按账号和功能执行的验证。`GenerationType.Text` 的真实序列化值是大写 `TEXT`，不是 `text`。其余声明值是 `AUDIO`、`IMAGE`、`VIDEO`、`SIMPLE_REMIX`。

已确认的可选字段包括 `tags`、`negative_tags`、`title`、`task`、`gpt_prompt`、`gpt_description_prompt`、`metadata`、`stream`、`is_regenerate`、`batch_size`、`continue_clip_id`、`continue_at`、`token`、`project_id` 等。反编译构造函数给出 `stream=false`、`is_regenerate=false`、`batch_size=2`；`task` 的数据模型默认值是 `null`，不能从字段名推定所有场景都必须发送 `task=""`。

REA 入向引用定位到真实构造调用者 `defpackage.pv7.j`。针对该类再次以 JADX `simple` 模式恢复后，`work/pv7-simple.java:588` 起的字段取值和 `:713` 的构造调用证明：内部状态 `v4i.a/b/c/d/e/f` 分别传入 `prompt/mv/tags/title/gpt_description_prompt/make_instrumental`；`task` 来自 `v4i.o`；`gpt_prompt` 在这条主路径传 `null`。`v4i.g` 映射文本、音频、图片、视频、简单 Remix 等生成类型。`:586` 构造 `GenMetadataSpec`，其中 create mode 按内部状态映射 `Simple` 或 `Custom`。`:717` 将该请求直接交给 `generateApiRunGenerationV2`。这是静态调用链证据，尚不证明线上接受任何特定输入组合。

`GenerationRequestSchema` 返回 `id`、`clips`、`metadata`、`major_model_version`、`created_at`，并声明可选 `status`、`batch_size`。应保存服务端返回的全部歌曲 ID，再查询每首歌曲至完成或失败；异步请求被接受不等于音频已完成。

模型列表使用 `RemoteModel`：`external_key` 是请求中的 `mv`，另有 `can_use`、`is_default_model`、`is_default_free_model`、能力与长度限制。APK 中的 `Model.SAMPLE` 是展示样例，不能当作用户账号可用模型或实时默认模型。

## 身份与会话

REA 完整恢复 `SunoAuthHeaderInterceptor.intercept`：从 `authTokenHolder` 取会话 JWT，设置 `Authorization: Bearer <JWT>`。`ApiMetadataHeaderInterceptor` 添加 `Content-Type: application/json` 和 `Accept-Language`；另有分析用 `anonymous-id` 和可选 `session-id` header，不能仅凭存在就断言它们是服务端生成必需条件。

`SessionJwtAuthenticator` 在首次认证失败且持有旧 JWT 时尝试刷新，并只在得到不同的新 JWT 后重发。Clerk 接口 `defpackage.vk2.h` 声明 `POST v1/client/sessions/{session_id}/tokens`，带 `_is_native`、`_clerk_js_version` 查询参数。静态分析并不能保证任意复制的旧 Cookie 都可刷新；用户登录状态、客户端凭据、有效期和服务端检查仍须实际验证。

## 下载授权与受保护播放流

下载授权请求模型为：

```json
{"item_id":"歌曲 UUID","item_type":"clip","surface":"share"}
```

`item_id` 和 `item_type` 必需，`surface` 可省略；枚举值包含 `clip`，以及 `surface="share"` / `"library"`。授权响应是 `{ok, already_unlocked, credit_deducted, reason?}`，因此授权调用可能扣减下载额度。下载 URL 响应是 `{ok, download_url?, status?, reason?}`。

明确的拒绝原因包括 `quota_exhausted`、`no_permission`、`invalid_asset`、`not_authorized`、`invalid_format`、`rate_limited`、`daily_limit_exceeded`、`internal_error`。实现应保留这些结果，不把拒绝当作可以猜测 CDN 路径继续下载的信号。

`common_media.drm.player.b` 使用 `AES/CTR/NoPadding`，`common_media.drm.player.c` 存在许可证/密钥 URI 检查及 `LicenseError`、`KeyUriError`。这证明 APK 有受保护播放路径。仅得到扩展名 `.m4a`、`media_urls` 或 `encoding` 字段不足以证明下载到了可播放文件；实现应使用服务端允许的下载地址并校验文件格式，不实现 DRM 绕过。

## 证据索引与可复核位置

REA 原始证据保存在 `work/`；可分享的精选静态片段和文件校验值保存在 `docs/evidence/apk-protocol.json`。其中不包含用户账号凭据。

| REA Evidence ID | 作用 |
| --- | --- |
| `ev_62e18766a4312a3df7e0b031f52789c501ab8fe45d052727d2d171698e2583de` | XAPK 静态包图与清单 |
| `ev_16bd33a30b777fd5d1064c5108be6adfe050627159918fe18373949c3bd9f31f` | XAPK 组成及各 APK 摘要 |
| `ev_02583905229e50ef1653bf1a35ac013bdf391ed3cb8de4954c09a7268329fdea` | 受限目录内的安全提取 |
| `ev_9f57cad736613adfa41f6e4dd20187681f7705f80e56b721b7edaa5f0c107340` | Android 包声明、版本、SDK、类数与清单 |
| `ev_fb3fe622d14f2107c7743f54bf845ddd1acbd8ad101d8b5a0a836185389ed422` | GenerateService 的实际方法成员与类型 |
| `ev_89f31f8ad457134ef5c242c5e2b16e2eb12db9a7cc18a8c276ea674be84f1d0e` | Bearer header 方法的完整静态源码 |
| `ev_571b1025886078a087004a45a4f094326420f11bb97b879eadae5a7a215be237` | GenParamsSpec 的静态入向引用 |

### 尚未由本次静态分析证明

- 服务器是否接受当前账号、模型、会话 JWT 或验证码；这取决于实时联调。
- 所有分享歌曲是否都允许下载，或是否有足够下载额度。
- 未提供有效用户会话时的歌曲生成成功；APK 不包含可离线运行的 Suno 生成模型。
- 未恢复方法及动态配置分支的全部行为。REA 类名全量搜索曾因 JADX inventory duplicate 检查失败；本报告改用明确类名定向分析，没有把失败当作“类不存在”。
