# 实现与验证记录

本记录对应 2026-11-17 的本地交付。源程序使用 Go 标准库，提供分享 URL 下载、描述生成、自定义歌词生成、任务查询和本地 HTTP 接口。

## 已验证的下载

- Windows 命令行接受 `suno2api.exe "https://suno.com/song/歌曲UUID"`，短链接另有自动解析路径和测试。
- 真实测试歌曲为公开分享的 Together in Respect，ID 为 `c1a16429-f3fe-4d99-bdcf-61ee13ed6f00`。
- 使用用户提供的正常 Suno 会话向官方播放许可接口取得许可，然后按 APK 播放器的协议解码播放音轨。没有把错误响应或加密原始字节当成音乐。
- 成品为 M4A 容器中的 Opus 音频，48 kHz、双声道、210 秒、3,480,824 字节。
- SHA-256：`2d48b18ad1bce28a39ccb8fdb265349755a7dbdf125ce52a2810553cffbcb5ec`。
- ffprobe 确认了容器和音频参数；ffmpeg 完整解码退出码为 0，没有解码错误。
- 该路径没有调用可能消耗下载额度的 `download/authorize`。

## 已完成的网页生成

在独立授权的一次网页协议验证中，当前 Cookie 配合其真实 Device-Id 调用正规 `c/check` 得到 `required=false`，随后仅一次 POST `https://suno.com/a/api/generate/v2-web/`。实际请求记录确认 `mv=chirp-goose`（v6-mini）、纯音乐，以及官方网页的普通 metadata 和本地 UUID；不发送安卓专用 stream/batch_size。

服务请求 ID 为 `efcc505e-a206-476b-ab50-5eba05a9b5aa`，额度 20→10。两首 v6-mini `67d3ee70-a875-4de2-8e57-d1d34a6b57ad`、`065338e2-e0bf-49f3-ab82-e573b7c3bfb6` 均已 `complete`，含可用媒体。状态由当前网页 feed/v3 取得；脱敏 actual wire body、ID、状态在 `work/live-generation-web.json`。

这证明当前默认网页 profile 可真实生成。用户也已在官网确认看到了标题为 Evening Teacup 的新歌。更新正常会话后，两首新歌均已从分享 URL 经正常播放许可下载为完整可播放文件，闭环完成。

| 歌曲 ID | 文件 | 时长 | 字节数 | SHA-256 |
| --- | --- | --- | --- | --- |
| `67d3ee70-a875-4de2-8e57-d1d34a6b57ad` | `outputs/generated/Evening Teacup-67d3ee70-a875-4de2-8e57-d1d34a6b57ad.m4a` | 193.92 秒 | 3,552,645 | `37d6265ea354ea5486970b0b35036a0e053021959e9e50927f3f6f4c6c6955d5` |
| `065338e2-e0bf-49f3-ab82-e573b7c3bfb6` | `outputs/generated/Evening Teacup-065338e2-e0bf-49f3-ab82-e573b7c3bfb6.m4a` | 179.60 秒 | 3,237,367 | `af701920c8972f0fa01bceaf8a82a75241d98154d932413a44ad562045b48a13` |

两首均由现有 ffprobe 确认为 MP4/M4A 容器、Opus 音频、48 kHz、双声道。现有 ffmpeg 分别完整解码到空输出，退出码均为 0，没有解码错误。脱敏结果保存在 `outputs/generated/validation.json`，不包含会话值、许可材料或临时媒体地址。

最初两次 CDN 下载曾超过两分钟后取消，残缺文件未发布。本次只读 Range GET 返回 206，0.86 秒内读满 65,536 字节；随后两首完整媒体下载分别约 1.61 秒和 2.41 秒完成，确认当前访问正常，没有据此增加产品重试或改动程序超时逻辑。本轮没有再次提交生成 POST。

## 保留的安卓验证任务

默认模型固定为用户要求的 v6-mini，实际提交参数为 `mv="chirp-goose"`，不自动选择账号不可用的付费模型。

初次生成在补齐验证码预检查前提交，返回的 v6-mini 歌曲为 `6e51d5be-145d-4c53-aa4f-6eb829cff72d`。随后对修正后的正常流程进行一次独立验证：先调用 `c/check`，收到 `required=false` 后，只提交一次生成请求。

修正版的实际请求正文已核验：`mv=chirp-goose`、`batch_size=1`、`make_instrumental=true`、`generation_type=TEXT`、`metadata.create_mode=simple`。服务端生成请求 ID 为 `03210acd-ecb1-43b5-94a5-08e22c0bd273`，对应的 v6-mini 歌曲 ID 为 `9379be2d-dcf6-4773-8f5e-1270f48cab21`。这次验证使剩余额度从 30 变为 20。

服务端两次都返回了额外的其他模型歌曲；客户端保留所有 ID，并只等待请求模型的结果。公开歌曲查询、禁止缓存的查询和 APK 的 `feed/v3` 查询均返回 `submitted`，尚无音频。不能把已接受请求、扣除额度或本地超时描述为生成完成，也不能据此确定持续排队的原因。本次没有自动重试任何生成 POST。

继续查询原任务：

```powershell
.\dist\suno2api.exe get --ids 9379be2d-dcf6-4773-8f5e-1270f48cab21 --wait --timeout 30m
```

完成后直接下载它：

```powershell
.\dist\suno2api.exe "https://suno.com/song/9379be2d-dcf6-4773-8f5e-1270f48cab21"
```

## 程序检查

`go test ./...`、`go vet ./...` 和 Windows 可执行文件构建通过。测试覆盖请求字段与 v6-mini 别名、验证码前置检查、任务轮询/取消/失败、异常响应保留已提交任务 ID、Cookie 文件更新、目的地址与凭据限制、播放许可绑定及 AES 随机访问边界、文件类型与长度检查、防覆盖发布、CLI 参数和 HTTP 接口边界。新增保活测试覆盖状态只读、前台生命周期与取消、同文件并发协调、到期前调度、有限失败退避、认证拒绝及浏览器验证分类、正常令牌与 Cookie 轮换、原子更新与手工文件变更冲突、认证材料独立文件及凭据发送边界。

HTTP 路由通过 httptest 真实 HTTP 请求验证。一个包含后台启动与终止进程的额外服务 smoke 命令被自动审批阻止，未执行，也未作为成功证据。当前记录不声称完成了该后台生命周期检查。CGO 关闭，未运行 race 检测。

## Cookie 续期

账号保活已实现为 `session status`、`session refresh`、`session keepalive`。正常生成、查询、下载和服务请求默认共用同一会话管理器，在到期前默认 60 秒按需换发；前台 keepalive 和服务运行期间的调度使用同样逻辑。提前刷新失败但原 JWT 仍有效时可继续认证，真实到期后清晰报错；生成 POST 不会因续期或认证失败而自动重发。

用户提供自己的认证域 Cookie 后，已使用最终产品命令 `dist/suno2api.exe session refresh` 完成一次真实正常续期。2026-11-17 北京时间 20:55:59 签发的新会话有效期延长至 21:55:59，返回令牌的 session、用户和 issuer 与原会话一致。API 会话文件及认证 Cookie 文件均完成正常更新，设备 Cookie 保留。随后使用更新后的会话执行 `billing` 只读请求成功，返回可解析模型配置。脱敏探针记录为 `work/session-refresh-product-probe.json`；产品 SHA-256 为 `558011d1323135d9f6bedd87f87facb243389dc804284798d0eebcb4f20e2ab1`。

本地忽略配置已指向实际存在的 `work/suno-session.token` 和 `work/suno-auth.cookie`。凭据与私有备份均不进入发行包。此处确认一次正常产品续期及后续认证成功，不据此承诺会话永久有效，也不把普通网页访问或 touch 当作续期成功。详细协议与 REA 证据见 [会话续期研究](session-lifecycle.md)。

自动 `internal/session.Manager.Run` 已在北京时间 21:25:59.003 按正常调度触发一次请求，旧到期时间 21:55:59 延长为 **22:25:58**。同账号、同 session、同 issuer 均核验通过，API 会话文件及认证文件更新、设备 Cookie 保留；传输层统计与 Manager 刷新计数都为 1，随后账号 billing 只读验证成功。验证器在第一次成功后取消，进程正常结束，没有留下持续后台。原始脱敏摘要为 `work/session-keepalive-product-probe.json`，打包精选副本在 `validation/session-keepalive-product.json`。

这次真实调度使用冻结前构建；其原始事件里保留了成功后主动取消引起的旧版 `stopped/temporary` 噪音，没有清洗证据。最终版本通过本地回归修复两点：计时器醒来重新检查已由其他请求更新的会话，避免重复续期；正常取消直接结束，不伪报临时故障。两项修复不改变线上续期协议，没有为复测重复发送真实刷新 POST。

## 交付范围

发行包包含程序、源码、配置样例、协议调查和本记录，不包含用户 Cookie、播放密钥、许可内容或带访问签名的音轨 URL。已验证的公开音乐样例单独保存于 `outputs/audio/`，两首真实新生成歌曲单独保存于 `outputs/generated/`，不放入源码发行包。
