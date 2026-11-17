# suno2api

用 Go 实现的 Suno 歌曲生成、分享链接下载工具，提供命令行和本地 HTTP 服务。默认使用当前官方网页协议，适配用户提供的网页 Cookie；同时保留用户提供的 Suno Android 1.97.1 安装包协议。依据见 [安装包分析](docs/reverse-engineering.md) 与 [网页协议核对](docs/web-generation-comparison.md)。

默认生成模型为 **v6-mini**，对应实际接口参数 `mv: "chirp-goose"`，符合当前 Free 账号配置。输入 `--model v6-mini` 或 `--model chirp-goose` 等价，不会自动切换到 v6 付费模型。

## 多账号网关与管理页面

```powershell
.\dist\suno2api.exe gateway
```

打开 `http://127.0.0.1:8080`。首次启动在 `data/gateway/admin.key` 生成管理员密钥，控制台只显示密钥文件的完整路径；在网页登录框输入文件中的密钥。管理员密钥、给调用方使用的网关 API 密钥、Suno 账号 Cookie 分别管理。账号和任务写入本机 `data/gateway/`，默认不会导入旧 Cookie 文件。

如需迁移当前配置指向的这个 Suno 账号，显式运行：

```powershell
.\dist\suno2api.exe gateway --import-current
```

导入只读取已指定的 `token_file` 与可选 `auth_cookie_file`，复制到网关自己的账号目录；重复导入同一会话会更新已有记录。Web 页面可添加更多账号、检查状态、管理调用密钥和查看任务。详细启动、密钥与接口说明见 [网关使用说明](docs/gateway.md)。

## 直接使用

Windows 可执行文件为 `dist/suno2api.exe`；源码构建不依赖第三方 Go 库：

```powershell
go build -o dist/suno2api.exe ./cmd/suno2api
```

输入一个 Suno 分享链接即可下载：

```powershell
.\dist\suno2api.exe "https://suno.com/s/SHARE_CODE"
.\dist\suno2api.exe download "https://suno.com/song/SONG_UUID" -o .\downloads\
```

支持 `suno.com/s/短码`、`suno.com/song/歌曲ID` 和旧 `suno.ai` 分享链接。保存文件名包含歌曲标题与 ID，避免覆盖现有文件。工具识别实际音频格式，播放音轨可能保存为 `.m4a`，不把它错误命名成 `.mp3`。可传输出目录；若指定完整文件名，其扩展名必须与实际格式一致。

公开直链可直接保存；需要登录的播放音轨使用账号的正常播放许可。账号无权取得的歌曲或已删除的歌曲会返回错误。默认不会调用可能消耗下载次数的下载授权；确实需要时可手动增加 `--authorize-download`。

## 登录凭据与代理

将你自己的 Suno Bearer/JWT 或原始 Cookie 保存到本地文件，例如 `session.token`，再指定：

```powershell
.\dist\suno2api.exe billing --token-file .\session.token
```

网页协议需要原始 Cookie 中的 `__session` 与 `ajs_anonymous_id`。歌曲 API 只收到会话 token 和实际 Device-Id，不收到整份 Cookie；只有单个 JWT 时可用 `--device-id` 指定自己的现有设备标识。也支持 `SUNO_TOKEN_FILE` 或 `SUNO_TOKEN`。已配置 Cookie 文件时，默认在会话临近到期时通过 Suno 正常认证接口续期并原子写回；每次请求仍重新读取文件，以支持手工替换。

在多账号网关中，如果你只有认证 Cookie，也可单独填写；它必须同时含有效 `__session` 和 `ajs_anonymous_id`。缺少其中任一项时请再填写网站 Cookie。

`config.local.json` 会自动读取，也可用 `--config` 指定其他配置文件。本工作目录已有仅含本地文件路径的忽略配置，指向本次提供的 `work/suno-session.token`。新环境可参照 `config.example.json` 创建自己的配置，不应把凭据文件放入代码交付包。

```json
{
  "protocol": "web",
  "model": "v6-mini",
  "token_file": "session.token",
  "auto_refresh": true,
  "refresh_before": "60s",
  "download_dir": "downloads",
  "listen": "127.0.0.1:8080"
}
```

代理优先顺序为 `--proxy`、`SUNO_PROXY`、标准 `HTTP_PROXY` / `HTTPS_PROXY`、`ALL_PROXY`，随后读取 Windows 当前用户已启用的系统代理。读取不会修改系统配置。支持 HTTP、HTTPS、SOCKS5；`--proxy direct` 可明确直连。输出只说明代理来源，不显示代理凭据。

## 账号保活

```powershell
.\dist\suno2api.exe session status --token-file .\session.token
.\dist\suno2api.exe session refresh --token-file .\session.token
.\dist\suno2api.exe session keepalive --token-file .\session.token
```

`status` 只检查本地会话状态、到期时间和能否尝试续期，不发网络请求。`refresh` 对当前同一 Suno 会话执行一次正常换发，成功后保留原 Cookie 文件中的设备标识等条目并原子更新会话值。`keepalive` 在前台等待，到到期前默认 60 秒才换发；按 Ctrl+C 停止，普通 `--timeout 10m` 不会使它在十分钟后退出。输出只有状态、时间和刷新计数，不显示 Cookie、JWT、用户 ID 或带 session ID 的 URL。

生成、查询、下载和 HTTP 服务默认共用同一个会话管理器，在认证请求前按需检查；HTTP 服务也在运行期间按同样计划续期。不会每次请求都刷新，不会在生成失败后自动重发生成 POST。提前刷新失败但原 JWT 尚有效时，普通请求可继续使用该 JWT；后台保活遇到明确认证拒绝会停止，令牌真正到期后认证请求会返回错误。可用 `--refresh-before 90s` 调整提前量，或 `--auto-refresh=false` / 配置 `"auto_refresh":false` 保留手工更新方式。

自动续期需要当前正常 Suno 会话及对应认证材料，单独一枚原始 JWT 只能在有效期内使用，不能保证续期。Ably 等实时消息 token 不能用于 Suno 登录或续期。服务器撤销、登录过期或拒绝认证时会停止并返回清晰状态；需要用户在 Suno 正常重新登录，再更新本地文件。程序只向已经核实的官方认证地址发送最小所需 Cookie，不修改浏览器或系统权限，不承诺永久在线。协议依据和实际验证边界见 [会话续期研究](docs/session-lifecycle.md)。

如果普通 Suno 页面 Cookie 不具备认证服务所需的会话材料，可将自己在 `auth.suno.com` 正常请求使用的认证 Cookie 另存为本地文件，用 `--auth-cookie-file .\auth.cookie`、`SUNO_AUTH_COOKIE_FILE` 或配置 `"auth_cookie_file":"auth.cookie"` 指定。该文件只用于已经核实的认证服务，不发给歌曲 API 或 CDN；无需把无关站点 Cookie 放入文件。收到服务器轮换的正常认证 Cookie 时，管理器会先保存认证文件，再更新 API 会话文件；不同文件发生手工变更时保留更新内容并报冲突。

本次已使用用户提供的正常认证 Cookie，通过最终程序执行一次真实 `session refresh`，确认同一账号会话延长、两个凭据文件更新、设备 Cookie 保留，并使用更新后的会话完成 `billing` 只读验证。本工作目录的忽略配置已指向实际认证文件，后续可直接运行 `session keepalive`；发行包不包含这些凭据。

自动调度也已真实验证：北京时间 2026-11-17 21:25:59 按计划只发送一次正常续期请求，会话到期时间从 21:55:59 延长至 **22:25:58**，两个文件更新后账号查询成功。验证在首次成功后主动停止，没有留下后台任务。最终版本另有本地回归测试，确保其他请求已续期时旧计时器不会重复刷新，以及 Ctrl+C/成功后取消不会报告假故障；此验证不承诺会话永久有效。

## 生成歌曲

简单描述生成，默认等待完成：

```powershell
.\dist\suno2api.exe generate --prompt "安静的夜晚，温柔钢琴" --instrumental --model v6-mini --batch-size 1
```

自定义歌词与风格，并在完成后保存音频：

```powershell
.\dist\suno2api.exe generate --lyrics-file .\lyrics.txt --style "acoustic folk" --title "晚风" --model v6-mini --download
```

`--protocol web` 默认走已核验的官方网页路径 `https://suno.com/a/api/generate/v2-web/`。网页请求使用官方普通客户端元数据、每次提交的新事务 UUID、当前会话 UUID，`Browser-Token` 仅为当前时间戳的普通 Base64 JSON，不涉及破解验证码或伪造签名。

`--protocol android` 保留安装包接口 `https://studio-api-prod.suno.com/api/generate/v2/`。`--batch-size` 只发送给安卓协议；当前网页 builder 没有这个字段，网页模式由服务决定数量。服务可能返回额外模型结果，程序保留全部 ID，并优先等待和下载所请求模型。

每次生成只提交一次 POST，使用账号现有额度。`--wait=false` 只提交并返回 ID；`--timeout 10m` 控制等待时间，轮询间隔默认 15 秒。超时表示本地停止等待，不能据此判断服务端任务已失败。可使用原 ID 恢复查询，不必再次生成：

```powershell
.\dist\suno2api.exe get --ids SONG_UUID
.\dist\suno2api.exe get --ids SONG_UUID --wait --timeout 10m
```

每次生成前先调用安装包中的正规 `c/check` 检查；若要求验证且没有用户完成验证后取得的真实 token，程序会停止且不提交生成。可用 `check-captcha` 单独检查。用户应在 Suno 完成验证，再按需提供 `--captcha-token`；工具不代答验证码，不购买订阅或自动重试生成请求。

## 本地 HTTP 服务

```powershell
.\dist\suno2api.exe serve
```

默认地址为 `http://127.0.0.1:8080`。

| 接口 | 用途 |
| --- | --- |
| `GET /health` | 健康检查 |
| `GET /download?url=分享链接` | 返回真正的音频附件，可直接另存 |
| `POST /api/download`，JSON `{"url":"分享链接"}` | 下载到配置的目录，返回路径、大小、SHA-256 |
| `POST /api/generate` | 提交生成，返回全部 ID 和所请求模型的 `requested_ids` |
| `GET /api/songs?ids=ID1,ID2` | 查询歌曲当前状态与媒体信息 |

```powershell
$body = @{ prompt = "gentle piano"; instrumental = $true; model = "v6-mini"; batch_size = 1 } | ConvertTo-Json
Invoke-RestMethod http://127.0.0.1:8080/api/generate -Method Post -ContentType application/json -Body $body
```

HTTP 生成参数支持 `prompt`、`lyrics`、`style`、`title`、`model`、`instrumental`、`custom`、`batch_size`。JSON 限制为 1 MiB，不接受客户端传入任意本地输出路径。文件下载接口只返回服务自己创建的临时文件。

服务拒绝跨站请求。需要本地 API 密钥时设置 `SUNO_API_KEY`，调用时使用 `Authorization: Bearer <本地API密钥>`。这个密钥与 Suno 会话不同。监听外部地址必须设置至少 16 字符的本地 API 密钥。

## 验证与当前结果

```powershell
go test ./...
go vet ./...
```

**当前网页协议的生成与下载已完整验证。** 单独授权的一次网页请求明确发送 `mv=chirp-goose`，服务扣除 10 点，两首 v6-mini 歌曲 `67d3ee70-a875-4de2-8e57-d1d34a6b57ad`、`065338e2-e0bf-49f3-ab82-e573b7c3bfb6` 均达到 `complete`，用户已在官网确认 Evening Teacup。两首已通过正常播放许可完整保存到 `outputs/generated/`，分别为 193.92 秒和 179.60 秒的 M4A/Opus 音轨，均通过 ffprobe 探测及 ffmpeg 完整解码。下载脱敏验证摘要为 `outputs/generated/validation.json`；音频单独交付，不包含在源码 ZIP 中。

公开分享链接下载已通过正常播放许可、媒体探测和完整解码检查。之前两次安卓协议验证的目标仍为 `submitted`，记录保留于 `work/live-generation.json`、`work/live-generation-corrected.json`，没有删除或自动重提。每个验证都有独立本地 guard，程序不会自动重试生成 POST。

更细的验证边界见 [生成说明](docs/generation-notes.md)、[下载说明](docs/download-notes.md) 与 [验证报告](docs/validation.md)。Cookie 的正常续期机制、当前文件能否自动保活的限制见 [会话续期研究](docs/session-lifecycle.md)。安装包中的附加请求元数据及其证据见 [请求头报告](docs/request-headers.md)。
