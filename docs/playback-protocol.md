# Suno 1.97.1 正常播放音轨协议

本报告补充用户明确要求的“通过播放音轨下载歌曲”路径。范围是使用用户自己的正常播放许可，按 APK 的现有播放器逻辑读取音轨。下载导出授权与播放许可是两个接口；它们的账号条件不能互相推定。以下均为指定 APK 的静态观察，线上账号是否能获得许可由实际服务器响应决定。

## 样本与调用链

分析的主 APK SHA-256 为 `01a454624aa3007a404b8555a4346bbca04f1f3c764645dfaaee22fc3d70e0fd`。原始包和 REA 身份证据见 `reverse-engineering.md`。这轮仅定向读取已有 JADX 输出，并对播放器 `b` 和许可仓库 `li5` 以 `simple` 模式恢复被省略的方法，没有运行 APK。

主调用链是：

1. `common_media.drm.player.b.c` 打开音轨数据源，同时启动正常许可获取。
2. `b.d` 从播放器内部 URI 读取 `drm_content_id` 和 `drm_content_type`，交给 `uh5`。
3. `uh5.invokeSuspend` 调用 `a47.C(contentId, contentType)`。
4. `a47.C` 调用 `li5.b`，取得缓存或网络许可；`li5.a` 发送服务器请求并解包许可。
5. `b.a` 初始化 AES-CTR，`b.read` 对底层数据源读出的字节连续解码后交给媒体解复用器。

## 播放许可请求

`LicenseService.fetchWrappedKey` 声明 `POST /api/mango/rights`。`NetworkingServicesModule.provideLicenseService` 使用传入的普通 Retrofit；DI `w74:1108` 使用 `n84.X`，与歌曲、分享、生成、订阅服务共用同一个实例。结合生产环境配置，实际地址是：

```text
https://studio-api-prod.suno.com/api/mango/rights
```

请求头使用正常 `Authorization: Bearer <用户会话 JWT>` 和 JSON Content-Type。歌曲请求体为：

```json
{
  "content_params": {
    "content_id": "歌曲 UUID",
    "content_type": "clip"
  },
  "version": "1.0.0"
}
```

`RemoteRightsBody` 的 `version` 默认是 `1.0.0`；`content_params` 必需，其中 ID 和类型均必需。此实现关注 `clip`；APK 还声明 `hook`、`visual_asset` 等类型，不能把它们随意替换以取得歌曲许可。

响应的 `RemoteRightsResponse` 包含可空字符串 `key`、`iv`。`li5.a` 明确检查 HTTP 状态、响应体、key、iv 和实际请求 JWT。非成功状态保留 HTTP 错误；缺字段直接失败，不允许猜测许可或使用其他内容的许可代替。

## 许可字符串解包

`li5.a` 从服务器响应关联的**实际请求**读取 Authorization，再去掉 `Bearer `。因此如果网络层先刷新 JWT 后重发请求，解包必须使用刷新后真正发送的 JWT，而不是最初读取的旧值。

令 `token` 是该请求的 JWT 字符串，`contentID` 是该请求使用的原始歌曲 ID 字符串：

```text
KEK = SHA-256(UTF-8(token))                 // 32 字节
AAD = UTF-8(contentID)
wrappedKey = Base64Decode(response.key)
wrappedIV  = Base64Decode(response.iv)

unwrap(wrapped):
  要求 len(wrapped) > 28
  nonce = wrapped[0:12]
  ciphertextAndTag = wrapped[12:]
  return AES-256-GCM.Open(KEK, nonce, ciphertextAndTag, AAD)

audioKey = unwrap(wrappedKey)
audioIV  = unwrap(wrappedIV)
```

`jb3.A` 明确使用 `AES/GCM/NoPadding`、12 字节 nonce、128 位认证标签和 content ID AAD。GCM 标签属于 `ciphertextAndTag` 的末尾 16 字节。解包失败意味着 token、content ID、许可响应或数据不匹配，应报错，不关闭认证校验。

这个过程只解开服务器按当前用户会话发回的正常许可，不需要客户端内置私钥、其他账号凭据或伪造请求。真实 key、IV 和会话 JWT 均不应写入报告、日志或交付包。

## Progressive 音轨字节

`b.a` 使用 `AES/CTR/NoPadding` 和解包后的音频 key、IV。IV 必须恰好 16 字节；音频 key 应满足 AES 支持的 16、24 或 32 字节长度。

`b.read` 直接把底层读到的整段密文字节交给 `Cipher.update`，要求输出长度等于读入长度。静态主路径没有“跳过文件前 N 字节”的额外封装，也没有按网络 read 长度重新初始化计数器。扩展名 `.m4a` 对应最终解码后的媒体容器；不能把未解码密文字节当作可播放 M4A。

从 offset 0 下载完整 progressive 音轨时，用 `audioKey`、`audioIV` 初始化一次 CTR，连续处理整个音轨即可。Go 的 `cipher.NewCTR` 使用大端计数器，与这段实现一致。

随机读取 offset `p` 时，APK 的初始化为：

```text
blockOffset = floor(p / 16)
skip = p % 16
counter = (bigEndian128(audioIV) + blockOffset) mod 2^128
初始化 AES-CTR(audioKey, counter)
丢弃 skip 字节密钥流
从媒体文件的真实 offset p 起处理密文
```

它通过从 IV 尾部逐字节进位实现大端加法，随后用 `cipher.update(new byte[skip])` 消耗块内前缀。网络分块读取不改变这个连续状态。

播放器还存在 `x-mango-hls://.../key/...` 的独立 HLS key 数据源；本轮目标是 `media_urls` 的 `delivery="progressive"` 路径，没有把 HLS 分段算法与 progressive CTR 混用。

## 实现验收与边界

- 只对 `encoding="1.0.0"` 且选择了适用 progressive 音轨的情况执行这个版本的解码；未来不同版本不能照猜。
- 许可必须成功且 GCM 校验通过；服务端拒绝当前账号时保留原错误。
- 读取音轨 CDN 时不附带账号 Cookie 或 Bearer token；账号凭据只发送给明确的 Suno API。
- 解码后检查媒体容器标识与实际可解码性，例如 M4A 的 `ftyp`，再作为下载成功交付。
- APK 未提供内容完整性的独立 MAC；CTR 解码完成和容器头正确不足以单独证明文件没有截断，应同时检查 HTTP 长度、下载结束和媒体解码结果。
- 静态代码证明客户端实现，不证明服务器当前允许指定 free 账号或任意分享曲目。线上联调应另记实际许可状态和产物检查结果。

精选静态证据保存在 `docs/evidence/playback-protocol.json`，含原文件 SHA-256、行号及定向源码片段。新生成的 `simple` 文件保留 `goto` 和反编译限制，不能称为原作者源码。
