# 多账号网关

网关把账号、正常会话续期、API 调用密钥和生成任务保存在本机文件里，不使用数据库。默认模型是 v6-mini，对应 `chirp-goose`；**默认协议是安卓 App 接口**（`studio-api-prod`，已真机验证），网页接口作为备用，可全局 `--protocol` 或按账号在管理页面切换。**直接运行二进制（不带参数）即启动网关 Web 应用**；`gateway` 子命令与之等价，原有 `generate`、`download`、`session`、`serve` 命令继续可用。

## 启动与登录

```powershell
.\dist\suno2api.exe
```

默认地址为 `http://127.0.0.1:8080`，数据目录为当前工作目录的 `data/gateway/`。首次启动会生成 `data/gateway/admin.key`，控制台显示它的完整路径，不显示密钥内容。打开管理页面，在登录框输入文件里的管理员密钥。使用期间不要把凭据文件、数据目录或管理员密钥发给调用方。

可以指定其他端口、私有数据目录或 API 风格：

```powershell
.\dist\suno2api.exe --listen 127.0.0.1:8181 --data-dir .\data\my-gateway --api-style both
```

使用已有管理员密钥时，把至少 32 字符且没有空白的密钥保存为本地文件，再使用 `--admin-key-file .\admin.key`；也可设置 `SUNO_ADMIN_KEY`。显式密钥文件优先于环境变量。该密钥用于管理页面，旧服务的 `SUNO_API_KEY` 不用于多账号网关登录。

默认只监听本机。若确实需要绑定外部地址，必须同时显式提供 `--listen` 和 `--admin-key-file` 或 `SUNO_ADMIN_KEY`，例如自行配置的内网地址。仅在配置文件中写外部地址不会开启远程监听。程序不配置公网映射、隧道或系统防火墙。

按 Ctrl+C 停止；HTTP 请求优雅结束，所有账号的后台保活和任务上下文随服务取消。`--timeout` / `request_timeout` 控制单次上游请求，不限制整个网关运行时长。

数据目录按记录拆分：

```
data/gateway/
  admin.key
  accounts/<账号ID>/account.json     账号设置、会话与额度缓存
  accounts/<账号ID>/session.cookie   该账号的 Suno 网站会话
  accounts/<账号ID>/auth.cookie      该账号的可选认证 Cookie
  keys/<密钥ID>.json                 调用密钥（仅保存哈希）
  jobs/<任务ID>.json                 生成任务原始记录
  files/<文件ID>.json                已保存音频的索引
  downloads/                         实际音频文件
```

每个账号的会话单独保存为文件，复制账号目录即可迁移该账号。旧版 `metadata.json` 会在首次启动时自动拆分到上述文件，并改名为 `metadata.json.migrated`。

## 添加账号与迁移现有账号

登录后在账号页面粘贴自己账号的 Suno 页面 Cookie，可设置便于识别的名字，并选择 API 协议（默认安卓 App 接口，网页接口为备用；两种协议共用同一份 Cookie）。网站 Cookie 中的 `__session` 是 API 会话，`ajs_anonymous_id` 是网页设备标识。认证 Cookie 是你在 `auth.suno.com` 正常认证请求中使用的 Cookie，可能包含 `__client`，用于同一账号会话的续期。

认证 Cookie 可以单独填写，前提是它同时包含有效的 `__session` 和 `ajs_anonymous_id`。网关会从这份 Cookie 提取 API 登录会话和网页设备标识，并保存认证 Cookie 供续期。如果缺少 `__session`，或缺少 `ajs_anonymous_id`，则需再填写网站 Cookie。两份 Cookie 属于不同账号时网关会拒绝保存。Ably 实时消息 token 不是 Suno 登录凭据。

接口接收 Cookie 文本，不接受调用方指定本机凭据路径。网关为各账号创建自己的受管文件，页面和账号列表不返回原 Cookie。添加或更新账号应使用自己已登录的正常会话材料；服务端要求重新登录或用户验证时，账号应进入相应状态。

**启用开关只决定账号是否参与新的生成任务。** 关闭后账号仍保留后台保活、余额检查和手动续期；已有任务、歌曲查询和音频下载继续使用原账号。只有「移除」才会归档账号并停止其维护，归档后可恢复。

首次迁移旧单账号配置必须明确选择：

```powershell
.\dist\suno2api.exe gateway --import-current --token-file .\session.token --auth-cookie-file .\auth.cookie
```

也可让这两个路径来自 `config.local.json` 或 `SUNO_TOKEN_FILE` / `SUNO_AUTH_COOKIE_FILE`。导入最多读取各 64 KiB，只读取明确指向的两个文件；不会扫描浏览器或其他账号。导入后网关维护自己的副本，后续旧文件与网关副本分别更新。重复导入同一账号会更新匹配记录，正常重新登录后的新会话也保留原账号归属，避免每次重启产生重复账号。

不带 `--import-current` 启动时，即使工作目录已有旧配置也不会导入任何凭据。

## API 风格

`--api-style`（配置项 `gateway_api_style`，环境变量 `SUNO_GATEWAY_API_STYLE` 或 `SUNO_API_STYLE`）可取：

- `suno`：只开放 `/api/*` 的 Suno 风格接口；
- `openai`：只开放 `/v1/*` 的 OpenAI 风格接口；
- `both`（默认）：两套同时开放。

公共接口统一使用 `Authorization: Bearer <网关API密钥>`。不要把管理员密钥或 Suno Cookie 当作调用密钥。管理页面「接入说明」会按当前风格显示示例。

### Suno 风格（`/api/*`）

`POST /api/generate` 接受 Suno 网页客户端使用的原生字段，并同步返回上游已接受的结果：

| 字段 | 说明 |
| --- | --- |
| `prompt` | 简单模式下可作音乐描述；`metadata.create_mode=custom` 时为歌词 |
| `gpt_description_prompt` | 简单模式的音乐描述 |
| `tags` | 自定义模式的音乐风格 |
| `title` | 自定义模式的歌曲标题 |
| `mv` 或 `model` | 模型键，默认 v6-mini / chirp-goose |
| `make_instrumental` 或 `instrumental` | 纯音乐，不生成演唱 |
| `batch_size` | 1 或 2 |
| `metadata.create_mode` / `custom_mode` / `custom` | `simple` 或 `custom` |
| `account_id` | 网关扩展；指定后只用该账号 |

纯音乐请求不会把描述或歌词送去演唱：

- 简单模式：`{"gpt_description_prompt":"雨夜氛围","mv":"v6-mini","make_instrumental":true}`
- 自定义模式：`{"tags":"ambient piano","make_instrumental":true,"metadata":{"create_mode":"custom"}}`，不要填 `prompt`/歌词；只填风格即可。

自定义演唱：`{"prompt":"[Verse]\n晚风...","tags":"acoustic folk","title":"晚风","mv":"v6-mini","metadata":{"create_mode":"custom"}}`。

返回结构与 Suno 一致，包含 `clips` 数组和网关任务 ID：

```json
{"id":"上游请求ID","clips":[{"id":"歌曲UUID","status":"submitted","model_name":"chirp-goose"}],"status":"submitted","requested_model":"chirp-goose","job_id":"网关任务ID"}
```

| 接口 | 用途 |
| --- | --- |
| `POST /api/generate` | 提交生成，返回 `clips` |
| `GET /api/feed?ids=ID1,ID2` | 查询歌曲；可按 `account_id` 指定原账号，否则按任务自动匹配 |
| `GET /api/clips/{id}` | 查询单首歌曲 |
| `GET /api/billing` | 额度、套餐与模型 |
| `GET /api/models` | 模型列表 |
| `GET /api/jobs`、`GET /api/jobs/{id}` | 网关任务列表/详情（扩展） |
| `GET /api/songs?account_id=…&ids=…` | 用指定原账号读取歌曲状态（扩展） |
| `POST /api/download` | JSON `{"url":"分享链接","account_id":"账号ID"}`，保存音频 |
| `GET /api/downloads/{id}/audio` | 下载已保存的音频附件 |

```powershell
$headers = @{ Authorization = "Bearer $env:SUNO_GATEWAY_KEY" }
$body = @{ gpt_description_prompt = "gentle piano"; mv = "v6-mini"; make_instrumental = $true; batch_size = 1 } | ConvertTo-Json
$created = Invoke-RestMethod http://127.0.0.1:8080/api/generate -Method Post -Headers $headers -ContentType application/json -Body $body
Invoke-RestMethod ("http://127.0.0.1:8080/api/feed?ids=" + $created.clips[0].id) -Headers $headers
```

### OpenAI 风格（`/v1/*`）

| 接口 | 用途 |
| --- | --- |
| `GET /v1/models`、`GET /v1/models/{id}` | 模型列表/详情 |
| `POST /v1/audio/generations` | 提交生成 |
| `GET /v1/audio/generations/{id}` | 查询状态与音频 URL |
| `GET /v1/audio/generations/{id}/content` | 通过原账号下载音频文件 |

提交字段：`model`、`prompt`、`lyrics`、`style`、`title`、`instrumental`、`custom`、`n`（1 或 2）、`wait`、`account_id`。`prompt` 默认是音乐描述；填写 `lyrics` 或 `custom=true` 时进入自定义歌词模式，纯音乐时歌词不会生效。`wait=true` 会等待歌曲完成（受请求超时限制）。

```powershell
$body = @{ model = "v6-mini"; prompt = "warm piano"; instrumental = $true; wait = $true } | ConvertTo-Json
Invoke-RestMethod http://127.0.0.1:8080/v1/audio/generations -Method Post -Headers $headers -ContentType application/json -Body $body
```

响应使用 OpenAI 约定字段：

```json
{"id":"网关任务ID","object":"audio.generation","created":1762900000,"model":"v6-mini","status":"processing","data":[{"id":"歌曲UUID","object":"audio","title":"","status":"submitted","url":""}]}
```

任务完成后 `status` 为 `completed`，`data[].url` 为可播放音频地址。

## 任务与安全

生成任务一经选定账号，后续状态查询、结果和错误归属该账号。上游生成 POST 不会因为超时、限流或失败而自动切换到另一账号重发；任务保持原 ID 供继续查询，避免重复消耗额度。Suno 风格的提交会等上游确认后返回；如果响应不明确，任务状态为 `submission_unknown`，应先在原账号核对，不要重提。

生成和下载都会使用原账号的正常登录与播放许可。遇到余额不足、需要验证、会话失效或下载许可拒绝时返回对应状态，不购买订阅、不伪造凭据、不代答验证码。仅检查连接时使用不创建任务的 `GET /health`。管理员接口使用独立登录会话和 CSRF 令牌，公共 API 密钥不能管理账号或创建其他密钥。

## 全局代理与运行日志

管理页面新增「系统设置」页：

- **全局代理**：留空自动检测（SUNO_PROXY、标准代理环境变量、Windows 用户代理），`direct` 强制直连，或填 `http://`、`https://`、`socks5://` 地址。保存后写入数据目录 `settings.json` 并立即重建所有账号连接（正在执行的任务会中断），对生成、续期、余额检查与下载统一生效；当前生效来源显示在页面上。启动命令行显式 `--proxy` 时以命令行为准并同步保存。
- **运行日志**：内存保留最近 1000 条，可按类型筛选（HTTP/登录/账号/任务/下载/设置/系统），同时按天写入数据目录 `logs/gateway-YYYYMMDD.log`（JSON 行），超过 14 天自动清理。日志只记录方法、路径、状态、耗时与安全映射后的错误消息，不记录 Authorization、Cookie、API 密钥或 Suno 凭据。接口：`GET /admin/api/logs?limit=&type=`（管理登录）。

## 配置与数据

优先级从高到低：命令行参数、进程环境变量、`.env`、JSON 配置文件、内置默认值。JSON 中的相对路径相对于该配置文件解析；`.env` 和命令行中的相对路径相对于当前工作目录。

`.env` 示例（复制 `.env.example`）：

```dotenv
SUNO_GATEWAY_LISTEN=127.0.0.1:8080
SUNO_GATEWAY_DATA_DIR=data/gateway
SUNO_GATEWAY_API_STYLE=both
SUNO_GATEWAY_ADMIN_KEY_FILE=admin.key
SUNO_MODEL=v6-mini
SUNO_REQUEST_TIMEOUT=10m
```

`config.json` / `config.local.json` 示例：

```json
{
  "protocol": "web",
  "model": "v6-mini",
  "request_timeout": "10m",
  "gateway_listen": "127.0.0.1:8080",
  "gateway_data_dir": "data/gateway",
  "gateway_admin_key_file": "my-admin.key",
  "gateway_api_style": "both"
}
```

`settings.json`（数据目录内，管理页「系统设置」写入）保存全局代理：

```json
{
  "proxy": ""
}
```

`gateway_admin_key_file` 可省略；省略后使用 `SUNO_ADMIN_KEY`，再没有时在数据目录生成管理员密钥。支持的环境变量包括 `SUNO_GATEWAY_LISTEN`、`SUNO_GATEWAY_DATA_DIR`、`SUNO_GATEWAY_API_STYLE`、`SUNO_GATEWAY_ADMIN_KEY_FILE`、`SUNO_ADMIN_KEY`、`SUNO_MODEL`、`SUNO_PROTOCOL`、`SUNO_PROXY`、`SUNO_TOKEN_FILE`、`SUNO_AUTH_COOKIE_FILE`、`SUNO_REQUEST_TIMEOUT`、`SUNO_AUTO_REFRESH`、`SUNO_REFRESH_BEFORE`。

`data/`、`work/`、本地配置、`.env` 和 Cookie 已被忽略，不进入源码发行包。备份网关时，应私下备份数据目录，并按凭据保护它；不要把包含账号和密钥的数据目录发布为公开资源。

代理选择沿用 `--proxy`、`SUNO_PROXY`、标准代理环境变量和已启用的 Windows 用户代理。控制台只报告代理来源，不显示代理凭据。
