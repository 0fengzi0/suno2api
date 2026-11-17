# Suno Cookie 与正常会话续期

结论：正常账号续期已经通过产品命令完成一次真实验证。用户补充认证域的正常 Cookie 后，程序在北京时间 20:55:59 成功换发同一账号、同一会话的新 JWT，有效期从 21:03:53 延长到 21:55:59，并安全更新两个本地文件；随后用新 JWT 读取账号模型信息成功。早先仅有 `__session` 的两次诊断没有成功，仍保留各自的证据边界。反复读取静态 Cookie 或请求歌曲接口本身不能续期；自动调度的持续运行效果须与这次手动命令验证分开报告。

## 当前文件的有限核验

检查的是用户主动更新的 `work/suno-session.token`，没有读取其他浏览器会话或凭据来源。

- 最初的 access 文件保留用户当前 `__session` 与既有 `ajs_anonymous_id`；初始两次诊断只发送 `__session`。
- 用户随后明确提供 `work/suno-auth.cookie`，普通文件 6481 字节，包含精确 `__client`、`__session` 和环境后缀会话 Cookie。程序只向 `auth.suno.com` 发送正常认证白名单中的 `__client` 与 session Cookie，其余分析、广告和统计 Cookie 不参与刷新请求。
- 刷新前，认证域 Cookie 的 session JWT 与 access 文件的 issuer、session、用户声明一致。旧到期时间为北京时间 2026-11-17 21:03:53；产品命令已把新到期时间更新为 21:55:59。文档和脱敏探针记录没有保存实际 session ID、用户资料或令牌值。
- 读取 JWT payload 只能检查声明和时间，不等于验证服务器签名，也不能据此保证整个登录会话可续期。
- Cookie 请求头快照不携带 Domain、Path、HttpOnly、Secure、SameSite、Expires 等属性。未在这个文件看到 `__client`，不代表浏览器在 `auth.suno.com` 域下也没有它；本次没有检查用户浏览器中其他 Cookie。

## 当前官方网页实际使用的流程

以下观察来自本次通过公开 GET 取得的 Suno 自己发布的 JavaScript，没有向这些静态资源发送用户 Cookie。

1. 认证路径解析模块把公开配置中的认证域解码为 `https://auth.suno.com`。模块 `305223.C` 接受等于 `suno.com`、`suno.fm` 或它们子域的 hostname；模块 `365943` 在该条件成立且认证域配置有效时使用认证域直连，否则使用当前 origin 的 `/api/clerk` 加同样的 `/v1/...` 路径。因此正式 `suno.com` 页面的通常选择是认证域直连，并非“直连 403 自动改代理”。官方同源代理的存在与其选择条件均已核对；本轮另一次代理诊断由用户明确授权。代码保留 Clerk 命名，不能把它当作任意第三方 Clerk 实例。
2. 正常换发接口是 `POST /v1/client/sessions/{session_id}/tokens`，`Content-Type: application/x-www-form-urlencoded`，空表单体，`credentials:"include"`。这段网页代码没有把现有 Suno API Bearer 放进 Authorization 头来当刷新凭据；浏览器依靠该请求目标对应的 Cookie 完成认证。
3. 成功响应读取 `jwt`。网页同时要求新 JWT 确实已经出现在浏览器读取到的 `__session` Cookie 中，并检查令牌未过期、session ID 和 user ID 与原会话一致。仅有一个 HTTP 200 或一个任意 JWT 字符串不足以证明续期成功。
4. 网页按 `exp - now - 30 秒` 安排换发，使用单个进行中的刷新任务合并并发请求，并在网络错误时有限退避。401、403 或返回了其他会话被当作终止条件；不会通过无限请求强留失效会话。
5. 页面聚焦时另有 `POST /v1/client/sessions/{session_id}/touch`，表单 `intent=focus`，最多约每五秒触发一次。该路径与换发 tokens 是不同动作；不能用 touch 成功来宣称 JWT 已延长。
6. 网页通过 Cookie 状态变化、visibility、online 和 BroadcastChannel 同步会话。保持官方网页正常登录可以让网页按上述机制自行维护登录；这不会自动改写本地手工复制出来的静态 Cookie 文件。

当前流程没有证明 `__session` 单独就足以续期，也没有证明必须出现某一个名称的 refresh token。只凭 Cookie 名称不能推断服务器是否接受；本轮的失败也不足以断定“必须缺少 __client”，更不能据此寻找浏览器里的额外私有材料。

## 本轮正常刷新实测

前两次请求使用同一个当前 session、最小 Cookie `__session`；第三次产品命令额外使用用户刚提供的同会话正常 `__client` 认证材料。三次均为空表单体、`Content-Type: application/x-www-form-urlencoded`、正常 Origin `https://suno.com`，没有 Bearer、没有跟随重定向、没有解决挑战、没有再次生成歌曲。

| 时间（北京时间） | 正常目标 | 结果与证据边界 |
| --- | --- | --- |
| 20:29:08 | `https://auth.suno.com/v1/client/sessions/{当前session}/tokens` | HTTP 403，存在 Set-Cookie；最初探针未保留 Content-Type、CF-Mitigated、WWW-Authenticate、错误体及 Cookie 属性，故**不能区分 WAF/challenge 与认证服务拒绝**，不能归因凭据不足。 |
| 20:36:19 | `https://suno.com/api/clerk/v1/client/sessions/{当前session}/tokens` | HTTP 400、text/html、Server=cloudflare；没有 CF-Mitigated challenge、WWW-Authenticate 或已检查的常见 HTML challenge 标志；非 JSON，没有可报告的错误 code；不能将它提升为明确的认证 403。 |
| 20:55:59 | 产品 `session refresh`，认证域直连、同 session、用户新认证 Cookie | CLI 退出码 0；产品校验新 JWT 与匹配的 session Set-Cookie，通过 issuer/session/用户/有效期绑定；两个凭据文件安全更新，随后账号 billing 查询成功。 |

代理响应 Set-Cookie 只有 `statsig_stable_id` 和 `ajs_anonymous_id`：前者 host-only、后者 Domain `.suno.com`，均 Path `/`、SameSite `lax`、有效期约一年，均不是空值或 Max-Age 清除标志；没有 `__session`，没有清除现有会话。它们的实际值未输出也未写回。脱敏属性保存在 `work/session-refresh-proxy-probe.json`。

前两次没有获得成功 JWT，也未更新文件；认证域 403 的详细原因未知，代理 400 的具体正文未保留，不补写推测原因。第三次验证使用刚构建并通过检查的正式产品 `dist/suno2api.exe`，不是临时脚本重写刷新算法；验证器只负责创建两份私有可恢复备份、调用一次产品命令并检查结果。

产品可执行文件 SHA-256 为 `558011d1323135d9f6bedd87f87facb243389dc804284798d0eebcb4f20e2ab1`。实际调用是 `session refresh --token-file work/suno-session.token --auth-cookie-file work/suno-auth.cookie --timeout 60s`。记录证明 `same_session=true`、`same_user=true`、`same_issuer=true`、`expiry_increased=true`、`device_cookie_preserved=true`，刷新状态为 `active`、刷新次数为 1，下次正常默认调度点是 21:54:59。私有备份仅留在忽略目录 `work/`，不得进入交付包。

随后使用 `--auto-refresh=false` 单次调用账号 billing 读接口，退出码 0、返回有效 JSON 和模型数组，证明持久化的新令牌可继续访问正常 API。没有重复生成或为了“保活”触发消费操作。

自动 `Manager.Run` 也已单独完成一次真实调度：北京时间 21:25:59.003 按计划请求，传输层真实刷新次数和 Manager 刷新计数均为 1。旧到期时间 21:55:59 延长至 **22:25:58**，同账号/session/issuer、两个凭据文件更新、设备 Cookie 保留均核验通过，随后 billing 查询成功。验证器使用正常 30 分钟提前量等待该调度点，未修改产品默认 60 秒提前量；首次成功后立即取消并正常退出，无持续后台进程。脱敏原始摘要为 `work/session-keepalive-product-probe.json`。

原始摘要保留旧构建成功后取消时附带的 `stopped/temporary` 事件；这不抹除前一项真实 `refreshed`、文件更新及账单查询证据。最终构建已用本地回归修复正常取消误报，以及旧计时器醒后在其他请求已续期情况下重复 POST 的问题。没有修改原始事件，也没有为这两项本地修复重新消耗一个线上刷新请求。手动实测的旧程序 SHA-256 保留原值；最终发行程序的校验值另列在包内 `SHA256SUMS.txt`。

## Android 安装包的补充证据

指定 APK 的 `defpackage/vk2.java` 第 45–46 行同样声明 `POST v1/client/sessions/{session_id}/tokens`，并带 `_is_native` 和 `_clerk_js_version` 查询参数。`defpackage/qj2.java` 第 21–47 行显示 Android 的认证网络层会从它自己的会话存储取得客户端认证字符串放入 Authorization，并在响应 Authorization 变化时调用后续更新处理。

这是 Android 原生认证存储与刷新路径的静态证据，不能据此把用户网页 Cookie 中的 `__session` 冒充原生客户端长期认证字符串，也不能把旧 APK 的所有原生参数强行套到当前网页上。应优先按当前官方网页已经证实的网页流程处理用户提供的网页会话。

## 当前可用做法与后续实现边界

Go 客户端每次认证请求重新读文件；用户提供正确的当前认证域 Cookie 后，产品已经完成一次真实刷新。自动续期实现应保留失败状态，并在服务器明确要求重新登录或遇到挑战时提示用户通过正常流程处理。写文件使用私有临时文件加原子替换，手动端到端验证前已保留可恢复的私有备份；不要将 Cookie 内容加入版本控制或报告。

本轮用户已授权对当前账号进行正常续期，授权范围不需要重复确认。但如果现有材料不能完成刷新，实现不得从其他应用或无关浏览器状态寻找凭据。只向已证实的 Suno 认证主机发送最小认证 Cookie，不能把整份带分析和广告标识的 Cookie 转发到所有 API、CDN 或第三方域。

实现需要独立 Cookie jar，处理 Domain/Path/Secure 和服务器 Set-Cookie；从同一个原会话读取 session ID，调用已证实 tokens 路径；检查新令牌 issuer、同一 session 与用户、尚未过期且 exp 延长，再原子更新认证文件。官网前端还要求新 `jwt` 出现在新 `__session` Cookie 中；没有观察到这里用 `document.cookie` 手动写入响应 jwt。程序若选择只接收响应 jwt 更新本地文件，应明确这是程序自己的适配策略，不能把 HTTP 200 无 Cookie 当作已经重现浏览器成功链。403 必须先标明是 challenge、认证拒绝或层次未知，均不自动绕过。429/网络失败最多按官网有限退避。服务端的会话过期、撤销与重新登录要求仍然有效，不能承诺永久在线。

播放许可的包裹密钥绑定签发该次许可时实际发送的 JWT。刷新后发起新的播放许可请求应使用新 JWT，但已经拿到的许可必须使用原请求 JWT 解包，不能因为 token 文件刚刷新就换一把 KEK；下载包已有对应测试。

## 精确来源与覆盖限制

- [官方网页 FAPI 与认证域配置脚本](https://suno.com/_next/static/immutable/chunks/19gdq4x6qxy9q.js)：本地 `work/session-web-js/19gdq4x6qxy9q.js`，SHA-256 `9b6fcd61d032c9c86d67a43c3aaa68e7ed8123845f1ee5941b442b2f5463adc2`。模块 `455325`（文本偏移约 40257）读取 `__session` / `__client_uat`；模块 `974710`（约 40607）定义带 credentials 的 POST/GET；模块 `442357` 定义 touch 后换发；模块 `365943`（约 45273）定义 `auth.suno.com` 与 `/api/clerk` 路径选择。
- [官方网页 Suno auth provider 脚本](https://suno.com/_next/static/immutable/chunks/2c5ye2i_zyo6b.js)：本地同名文件，SHA-256 `43c07e7000c87adf96a2b4b8e23bbe006d88243d0089f44589d73efb03e71dd0`。模块 `306271`（文本偏移约 45600 起），约 46909 为 tokens 请求，约 53324–53599 为过期与提前 30 秒刷新调度，约 54632 为 focus/touch。
- `work/jadx/sources/defpackage/vk2.java` 与 `qj2.java`：指定 APK 的反编译静态文件，分别证明原生 token endpoint 与认证头更新处理。APK 身份与静态分析范围见 `reverse-engineering.md`。
- `work/session-web-js/2me560eozqkeo.js` 模块 `305223`（约 19142）提供实际 hostname 选择条件：`.suno.com`、`.suno.fm`，不能把未恢复的 selector 描述为未知。
- REA 原件最小树 `work/session-refresh-sources/` 仅复制两份完整官方原脚本，未把切片冒充原件。一次 `analyze_javascript_application(detail=summary)` 的父 Evidence：`ev_c2a63604a75d97b84c4f3b67a33c909707f545eef92e3e0aab3afbe79c6af788`；summary `ev_084a41cbe089fe4490d2439a0356d73418e4f8eb2c9e34a1f396d772aa904f08`；module page `ev_834408c6561fc2db1ab0f32c5506e5f4943a516ea91e196ddf84f49f4a5aba3d`；原文件 item `ev_2145a380c45d776962c179aed6dc0e12587106abc874cc275d9a6acdf9495b60`。两个文件 AST 均解析成功、无 parse failure，但语义图仍有 4151 个 unknown，不能把静态图称为完整运行证明。完整静态包保留于 `work/rea-session-refresh-bundle.json`。
- 脱敏线上证据：`work/session-refresh-probe.json`、`work/session-refresh-proxy-probe.json`、`work/session-refresh-product-probe.json`；精选可交付证据：`docs/evidence/session-refresh.json`。

本次核验确认了已发布的正常续期协议、官方 hostname 路由条件，并在用户补充同会话正常认证 Cookie 后，完成手动产品刷新、一次真实自动调度、双文件持久化及新 JWT 账号查询。验证在首次自动成功后结束，不据此承诺永久在线；公开网页脚本也可能随部署变化，运行前应核对实际版本。
