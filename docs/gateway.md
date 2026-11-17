# 多账号网关

网关把账号、正常会话续期、API 调用密钥和异步生成任务保存在本机。默认模型是 v6-mini，对应 `chirp-goose`。原有 `generate`、`download`、`session`、`serve` 命令继续可用；`gateway` 使用独立的多账号管理界面和调用密钥。

## 启动与登录

```powershell
.\dist\suno2api.exe gateway
```

默认地址为 `http://127.0.0.1:8080`，数据目录为当前工作目录的 `data/gateway/`。首次启动会生成 `data/gateway/admin.key`，控制台显示它的完整路径，不显示密钥内容。打开管理页面，在登录框输入文件里的管理员密钥。使用期间不要把凭据文件、数据目录或管理员密钥发给调用方。

可以指定其他端口或私有数据目录：

```powershell
.\dist\suno2api.exe gateway --listen 127.0.0.1:8181 --data-dir .\data\my-gateway
```

使用已有管理员密钥时，把至少 32 字符且没有空白的密钥保存为本地文件，再使用 `--admin-key-file .\admin.key`；也可设置 `SUNO_ADMIN_KEY`。显式密钥文件优先于环境变量。该密钥用于管理页面，旧服务的 `SUNO_API_KEY` 不用于多账号网关登录。

默认只监听本机。若确实需要绑定外部地址，必须同时显式提供 `--listen` 和 `--admin-key-file` 或 `SUNO_ADMIN_KEY`，例如自行配置的内网地址。仅在配置文件中写外部地址不会开启远程监听。程序不配置公网映射、隧道或系统防火墙。

按 Ctrl+C 停止；HTTP 请求优雅结束，所有账号的后台保活和任务上下文随服务取消。`--timeout` 控制单次上游请求，不限制整个网关运行时长。

## 添加账号与迁移现有账号

登录后在账号页面粘贴自己账号的 Suno 页面 Cookie，可设置便于识别的名字。页面 Cookie 中的 `__session` 是 API 会话，`ajs_anonymous_id` 用于官网正常设备标识。认证 Cookie 是你在 `auth.suno.com` 正常认证请求中使用的 Cookie，可能包含 `__client`；它用于同一账号会话的正常续期。两者用途不同，实时消息 Ably token 不能作为登录材料。

接口接收 Cookie 文本，不接受调用方指定本机凭据路径。网关为各账号创建自己的受管文件，页面和账号列表不返回原 Cookie。添加或更新账号应使用自己已登录的正常会话材料；服务端要求重新登录或用户验证时，账号应进入相应状态。

首次迁移旧单账号配置必须明确选择：

```powershell
.\dist\suno2api.exe gateway --import-current --token-file .\session.token --auth-cookie-file .\auth.cookie
```

也可让这两个路径来自 `config.local.json` 或 `SUNO_TOKEN_FILE` / `SUNO_AUTH_COOKIE_FILE`。导入最多读取各 64 KiB，只读取明确指向的两个文件；不会扫描浏览器或其他账号。导入后网关维护自己的副本，后续旧文件与网关副本分别更新。重复导入同一账号会更新匹配记录，正常重新登录后的新会话也保留原账号归属，避免每次重启产生重复账号。

不带 `--import-current` 启动时，即使工作目录已有旧配置也不会导入任何凭据。

## 调用密钥与任务

在管理页面创建网关 API 密钥，记录创建时显示的一次性密钥。调用方发送 `Authorization: Bearer <网关API密钥>`；不要把管理员密钥或 Suno Cookie 当作网关调用密钥。

生成任务一经选定账号，后续状态查询、结果和错误归属该账号。上游生成 POST 不会因为超时、限流或失败而自动切换到另一账号重发；任务保持原 ID 供继续查询，避免重复消耗额度。账号停用应阻止新任务进入该账号，并取消其后台续期；恢复后按正常会话状态重新运行。

生成和下载都会使用原账号的正常登录与播放许可。遇到余额不足、需要验证、会话失效或下载许可拒绝时返回对应状态，不购买订阅、不伪造凭据、不代答验证码。

网关公共接口均要求网关 API 密钥：

| 接口 | 用途 |
| --- | --- |
| `POST /api/generate` | 创建生成任务，返回 `202 {"job": ...}` |
| `GET /api/jobs` | 查询任务列表 |
| `GET /api/jobs/{id}` | 查询原任务的账号、歌曲 ID 和状态 |
| `GET /api/songs?account_id=账号ID&ids=歌曲ID` | 用指定原账号读取歌曲状态 |
| `POST /api/download` | JSON `{"url":"分享链接","account_id":"账号ID"}`，返回已保存文件的信息 |
| `GET /api/downloads/{id}/audio` | 下载保存的音频附件 |
| `GET /download?url=分享链接&account_id=账号ID` | 直接取得音频附件 |

`/api/generate` 使用平铺参数：`prompt`、`lyrics`、`style`、`title`、`model`、`instrumental`、`custom` 和可选 `account_id`。不指定账号时由网关从已启用且可接任务的账号选择；指定后只使用该账号。先保存返回的网关任务 ID，再轮询任务状态，避免因等待超时再次创建。

```powershell
$headers = @{ Authorization = "Bearer $env:SUNO_GATEWAY_KEY" }
$body = @{ prompt = "gentle piano"; instrumental = $true; model = "v6-mini" } | ConvertTo-Json
$submitted = Invoke-RestMethod http://127.0.0.1:8080/api/generate -Method Post -Headers $headers -ContentType application/json -Body $body
Invoke-RestMethod ("http://127.0.0.1:8080/api/jobs/" + $submitted.job.id) -Headers $headers
```

以上创建操作会使用被选 Suno 账号的现有生成额度。仅检查连接时使用不创建任务的 `GET /health`。网页内管理员接口使用独立登录会话和 CSRF 令牌，公共 API 密钥不能管理账号或创建其他密钥。

## 配置与数据

```json
{
  "protocol": "web",
  "model": "v6-mini",
  "gateway_listen": "127.0.0.1:8080",
  "gateway_data_dir": "data/gateway",
  "gateway_admin_key_file": "my-admin.key"
}
```

`gateway_admin_key_file` 可省略，省略后使用 `SUNO_ADMIN_KEY` 或数据目录内生成的管理员密钥。配置中的相对目录和密钥文件路径相对于该配置文件解析；命令行路径相对于当前目录解析。

`data/`、`work/`、本地配置和 Cookie 已被忽略，不进入源码发行包。备份网关时，应私下备份数据目录，并按凭据保护它；不要把包含账号和密钥的数据目录发布为公开资源。

代理选择沿用 `--proxy`、`SUNO_PROXY`、标准代理环境变量和已启用的 Windows 用户代理。控制台只报告代理来源，不显示代理凭据。
