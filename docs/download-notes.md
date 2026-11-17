# 分享链接下载实现与验证

实现位于 `internal/download`，使用 Go 标准库。支持 `https://suno.com/song/<UUID>`、`https://suno.com/s/<shortcode>` 及相应 `www`、`suno.ai` 地址。短链接通过安装包内已证实的 `GET share/code/{shortcode}` 解包；也支持网页跳转及 canonical/og:url 回退。只接受歌曲分享，不将歌单或其他内容误作歌曲。

## 下载流程

1. `GET https://studio-api-prod.suno.com/api/clip/{id}/` 取得标题、状态及音频信息。
2. 如果歌曲提供公开、未经编码的 `audio_url` 或 progressive `media_urls`，下载该文件。
3. 如果 progressive 播放音轨带 `encoding:"1.0.0"`，向同一官方 API 的 `POST /api/mango/rights` 请求账号的正常播放许可，使用 APK 播放器相同算法解包许可并对整个音频连续解码。许可请求体为 `{"content_params":{"content_id":"<id>","content_type":"clip"},"version":"1.0.0"}`；保存实际播放格式，通常为 M4A/Opus。该路径不调用付费下载授权接口。
4. 正常播放许可被拒绝时直接报错；不会伪造账号、令牌或许可。播放路径不可用时，原有正式下载接口 `GET download/clip/{id}?format=mp3` 仍作为兼容回退保留；仅当接口明确返回 `not_authorized` 且调用者启用 `AllowAuthorize` 时，才发送一次可能扣下载额度的 `POST download/authorize`。

官方返回的编码播放数据只有在账号真实取得该歌曲的正常许可并成功解码后才会发布为音乐文件。`audio_url` 指向 `/api/forbidden` 和 `sil-100.mp3` 静音占位不会被当成歌曲。编码原始字节不会被误标为已完成的 M4A 文件。

## Go 接口

`NewClient(Options)` 返回客户端。`Resolve(ctx, shareURL)` 返回 `Track`；`Download(ctx, shareURL, outputPath)` 完成解析及保存；`DownloadTrack(ctx, track, outputPath)` 保存一个已经解析的、来自允许 CDN 的音轨。

`Options` 包括 `Token`、`TokenFile`、`ProxyURL`、`HTTPClient`、`MaxBytes`、`AllowAuthorize` 和授权下载请求使用的 `Format`（默认 mp3）。库本身默认不发送可能扣下载额度的 POST。`TokenFile` 每次 API 请求时重新读取，以便外层更新登录状态。Token 只发送到固定官方 API 域名，网页及 CDN 请求均不携带登录凭据。

`Track` 的 JSON 字段为 `id,title,share_url,format,source,encoding,credit_deducted`。`Result` 包括 `track,path,bytes,sha256`。`AudioURL` 仅保留在 Go 内存结构中，JSON 序列化会忽略它；许可 key、IV、JWT 和带临时访问参数的音频地址均不会进入结果 JSON。

输出文件名会去除 Windows 不允许的字符。空输出路径默认写入 `downloads/标题-UUID.实际格式`；提供目录则写入目录；提供文件名时扩展名必须符合音频实际格式。文件先写临时文件，再通过不覆盖已有文件的原子发布完成。下载完成前检查大小、文件签名和已声明的内容长度；HTML、JSON、编码数据、截断响应及超限响应不会成为最终歌曲文件。

默认直连模式会拒绝私有/回环网络地址，且只允许指定 Suno 页面、API 和已观察到的官方 CDN 域名。显式代理模式仅使用调用者提供的代理，不修改系统网络设置，仍保留目标域名、HTTPS、重定向及凭据隔离检查。Windows 当前用户的系统代理不会被 Go 自动读取，外层可读取并传入 `ProxyURL`，或直接注入配置好的 `HTTPClient`。

## 2026-11-17 验证记录

- 对官方公开歌曲页面和 `GET /api/clip/c1a16429-f3fe-4d99-bdcf-61ee13ed6f00` 的真实请求成功，返回标题 `Together in Respect`、`status:complete` 和 `audio_url:/api/forbidden`。
- 该接口返回 `media_urls:[{"url":"https://d2lwuy8qc234o3.cloudfront.net/1/clip/<id>.m4a","content_type":"m4a-opus","delivery":"progressive","encoding":"1.0.0"}]`。实际取回 3,480,824 字节，开头不具备 MP3/M4A/Ogg 音频签名；没有将它认定为可播放下载结果。
- 使用本任务提供的登录凭据及本机已有代理调用正式 `GET download/clip/{id}?format=mp3`，真实返回 `HTTP 200,{"ok":false,"reason":"no_permission"}`。这是其他作者歌曲的权限拒绝；没有为该歌曲发送授权 POST，没有声称真实分享下载成功。
- 用户随后明确要求通过播放音轨保存歌曲。已使用同一账号真实调用 `POST /api/mango/rights` 取得上述公开歌曲的正常播放许可，成功解包并连续解码实际 M4A 文件；从分享 URL 到最终可播文件的完整流程成功，未调用付费下载授权 POST。
- 成品为 `Together in Respect-c1a16429-f3fe-4d99-bdcf-61ee13ed6f00.m4a`，3,480,824 字节，SHA-256 `2d48b18ad1bce28a39ccb8fdb265349755a7dbdf125ce52a2810553cffbcb5ec`。本机现有 ffprobe 确认 MP4/M4A 容器、Opus 音频、48 kHz、双声道、210 秒；本机现有 ffmpeg 完整解码到空输出，退出码 0，无错误。
- 单元测试覆盖短链解包、公开歌曲解析、正式授权请求与额度标志、拒绝未经启用的授权 POST、禁止向 CDN 传登录凭据、域名和 HTTPS 验证、跨域重定向拒绝、HTML/超限拒绝、不覆盖已有文件、M4A 扩展名及 Next.js 页面解析，并新增 AES-CTR NIST 向量、任意字节偏移、IV 进位/溢出、非块对齐流式读取、GCM 的歌曲/账号绑定、许可拒绝不取媒体、请求后 token 文件变动和结果 JSON 不含秘密。上述测试通过。
- 自己账号新生成歌曲的下载由主任务在歌曲完成后另行验证，不能用公开歌曲成功声称新生成歌曲也已成功。

APK 的路由和请求体来源为 `DownloadAuthorizationService`、`RemoteDownloadAuthorizeBody`、`RemoteDownloadAuthorizeResponse`、`RemoteDownloadUrlResponse`、`ShareLinkService`；完整静态证据由同目录的逆向报告记录。网页和实时响应属于独立运行时证据，未将静态观察写成执行成功。

播放许可及解码的精确静态来源在 `playback-protocol.md`：实际许可请求所发送 JWT 的 SHA-256 作为 KEK；许可中 `key`、`iv` 各为标准 Base64 编码的 `nonce12 || AES-GCM密文 || tag16`，AAD 是 UTF-8 歌曲 ID；解包后用 AES-CTR/NoPadding 解媒体，IV 为 16 字节，随机偏移按大端计数器推进。代码从该次实际 HTTP request header 获取 JWT 绑定许可，不会错误使用后续已刷新的 token。
