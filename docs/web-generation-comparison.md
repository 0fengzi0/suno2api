# 当前官网生成协议与 Android 1.97.1 对比

本报告仅用公开、未携带 Cookie 或 Authorization 的 HTTPS GET 获取 `https://suno.com/create` 与其公开脚本；未运行浏览器挑战、未提交歌曲，也未读取用户其他账号秘密。静态代码足以确认网页提交主链及请求构造，不能证明新协议一定能修复已有 `submitted` 任务。

## 实际提交入口

官网 `/create` 页面公开 HTML 为 141,033 字节，SHA-256 `840756bd699c9130b809438686f434e3c528cac90a6a6d131f67620255372cb8`。页面脚本清单与原文件保存在 `work/web-create/`。

网页提交不是仅替换 Android 路径后缀：公共 API 客户端模块 `79076` 的 `baseUrl` 明确为 `https://suno.com/a`；生成 Hook 模块 `628043` 中 `z()` 调用 `session.apiClient.POST(F.Hg,{body:U,...})`，`F` 是模块 `725516`，其导出 `Hg` 是 `/api/generate/v2-web/`。因此网页实际提交地址为：

```text
POST https://suno.com/a/api/generate/v2-web/
```

Android 主链则是 `POST https://studio-api-prod.suno.com/api/generate/v2/`。当前用户持有网页会话时，可以提供显式网页协议选项；不能把两种客户端请求形状混为同一条已验证线路。

## 官网请求头

模块 `79076` 在每次 fetch 前调用正常 header helper：

| Header | 真实来源 |
| --- | --- |
| `Authorization` | 已登录会话 `getToken()` 的 Bearer token |
| `Device-Id` | 模块 `42305` 导出 `M`；读取 localStorage 或 Cookie `ajs_anonymous_id`，URL 解码、移除外围引号，并过滤为字母、数字、连字符 |
| `Browser-Token` | 本地时间戳 wrapper，见下文；不是从其他账号取得的秘密 |
| `Accept-Language` | 页面当前语言；无有效语言时默认 `en` |
| `Referring-Pathname` / `Referring-Url-Params` / `Referring-Origin` / `Referring-External-Referrer` | 仅当当前页面对应 URL 查询参数有值时加入 |

`Browser-Token` 公开构造为：

```text
inner = JSON.stringify({timestamp: Date.now()})
Browser-Token = JSON.stringify({token: btoa(inner)})
```

其中 `Date.now()` 是当前毫秒时间，`btoa` 对该 ASCII JSON 做标准 Base64；没有签名、账号密钥或固定伪造证明。应照正常本地元数据生成，不能把它称为已验证能绕过任何检查。

`Device-Id` 当前源码明确取 `ajs_anonymous_id`，并没有把导出的 `suno_device_id` 字符串直接作为这个 header 的 Cookie 查找名。应优先使用用户已有的真实 `ajs_anonymous_id`，没有映射证据时不要把其他 Cookie 改名填入。此网页主链未观察到 `x-suno-client="web"` 的构造；安卓三项 `x-suno-*` 不是网页必发字段证据。

## 验证预检查

网页 `session.getCaptchaTokenIfRequired` 使用同一个客户端：

```text
POST https://suno.com/a/api/c/check
Content-Type: application/json
{"ctype":"generation"}
```

它读取 `required` 和 `captcha_version`。模块 `902224` 明确常量 `1`、`2`；UI 中 version 2 走 Turnstile，另一路 version 1 为 hCaptcha。若返回 `required=false`，helper 返回 `null`；若需要验证，则等待用户正常完成当前挑战。取得真实 token 才将 `token_provider` 设为实际 `generationCaptchaVersion`，否则其值为 `null`。

这比 Android 的仅 `required` 返回模型多一个 provider/version 选择。Go 可以保留该字段并接收用户提供的真实验证结果；不能猜 token，也不能自动完成挑战。网页检查请求异常时会转入正常挑战流程，程序实现若不能展示挑战应明确报错停止。

## 普通文本生成 body

公开 builder 是模块 `168152` 导出 `e()`，由提交 Hook 传入 token、create session token、账号 plan ID、参数、transaction UUID、session 和 provider。对于无音频/图片/视频引用的 Simple 模式，下列字段由 builder 明确赋值；这是“客户端一定构造”的证据，不代表服务端 schema 对所有字段都 required。

```json
{
  "generation_type": "TEXT",
  "mv": "chirp-goose",
  "prompt": "",
  "gpt_description_prompt": "用户歌曲描述",
  "make_instrumental": true,
  "transaction_uuid": "本次提交的本地 UUIDv4",
  "token": null,
  "token_provider": null,
  "user_uploaded_images_b64": null,
  "override_fields": [],
  "cover_clip_id": null,
  "cover_start_s": null,
  "cover_end_s": null,
  "persona_id": null,
  "artist_clip_id": null,
  "artist_start_s": null,
  "artist_end_s": null,
  "continue_clip_id": null,
  "continued_aligned_prompt": null,
  "continue_at": null,
  "metadata": {
    "web_client_pathname": "/create",
    "create_mode": "simple",
    "user_tier": "账号实际 Free plan 的 id",
    "create_session_token": "当前创建会话的本地 UUIDv4",
    "disable_volume_normalization": false,
    "lyrics_model": "default"
  }
}
```

`make_instrumental` 来自用户选择；不是无条件 true。模型映射在当前 builder 中直接写明 `V6_MINI -> chirp-goose`、`V6 -> chirp-hawk`。

`user_tier` 的源码来源是 `session.sub.plan.id`，或 `session.sub.plans` 中 `plan_key === Free` 的 `id`；不是直接猜测为字符串 `free`。如果实际状态缺少该值，它是 `undefined`，JSON 序列化会省略。`web_client_pathname` 来自真实页面 `window.location.pathname`；相当于 CLI 所选 `/create` 协议表面时，可以明确记录这一值。

`create_surface`、`is_max_mode`、`is_mumble`、`from_studio_project_id`、`batch_offset` 等键在 metadata 对象表达式中构造，但其值取实际参数，值为 `undefined` 就会从 JSON 中省略。`edit_session_id`、`duration`、`lyrics_project_id`、`lyricist_id`、项目 ID 同理。`task` 初始为 undefined，普通无引用文本请求可省略。控制滑块、persona、参考音轨、定制模型与高级 task 都有独立条件，本轮不扩展这些功能。

Simple 模式的 `metadata.lyrics_model` 为用户选择或常量 `725516.c_`，该常量已核实为字符串 `default`。Custom 模式则设置 `title`、`tags`、`negative_tags`，`prompt` 是用户歌词，`create_mode="custom"`；歌词主题存在时才设置对应 description/lyrics model。普通网页 builder 没有恒定 `stream` 或 `batch_size` 字段；不能把 Android 的默认值当作网页服务端必需规则。

## UUID 是本地标识

`create_session_token` 由模块 `383054` 的 React `useState(() => uuid())` 创建，并通过模块 `509924` 的 Context Provider 注入；当前 create UI 会话保持同一个值。`transaction_uuid` 来自模块 `277565` 的 transaction logger，建立该 logger 时直接调用同一个 UUID 函数。

两者最终都指向模块 `197895`：优先使用 `crypto.randomUUID()`，否则生成 16 字节随机值并设置 RFC 4122 version 4、variant 位，输出小写 UUID。它们不是要向官方 POST 获取的签名、服务器 session credential 或可猜测权限令牌。CLI 可用自己的安全随机 UUIDv4 模拟本地会话/提交标识，仍应每次新提交独立 transaction UUID，现有已提交任务不重新提交。

## 提交后的实际动作与查询

提交成功返回 `clips` 后，Hook 做本地缓存更新、登记 clip/request ID、启动查询/订阅、刷新订阅信息和记录正常 UI 事件。已检查的普通生成主链没有第二个“启动生成”POST，也没有强制队列注入。

当前官网按歌曲 ID 查询的确定调用为：

```text
POST https://suno.com/a/api/feed/v3
{"filters":{"ids":{"presence":"True","clipIds":["歌曲ID"]}},"limit":1}
```

响应使用 `clips` 数组。`presence` 是大写首字母字符串 `True`。单曲页面另有 `GET https://suno.com/a/api/clip/{clip_id}`，源码路径没有末尾 slash。轮询读查询不能重新提交生成。

## 覆盖限制

- 此次只获取公开静态资源；个别无关脚本下载失败，所选提交 Hook、body builder、API wrapper、验证码 helper、UUID 与查询模块均已核读。
- 已恢复的网页 profile 包含同源 API base、正常头、body、captcha provider 和查询路径，足以实现一个显式可选协议；并未测试该 profile 的真实生成结果。
- 不能由“Android v2 扣点且 submitted”推断“v2-web 一定能完成”，也不能把旧批次改用新 POST 重试。旧任务状态仍以已有 ID 的服务器查询为准。
- 不把真实 token、Cookie、设备 ID、验证 token 或用户内容写入精选证据。公开脚本字节、SHA-256、字符 offset 及关键片段保存在 `docs/evidence/web-generation.json`。
