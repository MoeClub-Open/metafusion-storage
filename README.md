# MetaFusion Storage

MetaFusion 物理资产归档与下载中枢：文件本体、内容寻址、直传与绑定。
对应拆分基准见主仓库 [docs/architecture/service-split-migration.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/service-split-migration.md) 的 P1 阶段。

## 职责边界

- **拥有**：物理文件、sha256 内容寻址、对象存储直传与预签名、文件→实体的绑定、下载与预览的访问控制。
- **不拥有**：作品/专辑/曲目等目录数据（不复制、不 JOIN 目录库）、收录位置（页码/时间码属目录侧 `locator`）。
- **依赖**：元数据目录服务（实体身份与可见性）、账号服务（令牌验签和 PAT 内省）。

存储通过 `GET /api/catalog/entities/{id}/identity` 一次获得 canonical、kind 与完整历史别名，响应必须确认 `complete=true`。可见性、上传绑定和下载判定共用此协议；明确的 `404 not_found` 表示不可见，接口缺失、协议错误和上游不可用按依赖故障处理，不回退到旧实体/resolve 查询。实体合并不改写存储绑定，读取按别名集合聚合。
用户会话 JWT 在本地验签，PAT 通过 `AUTH_URL`（账号服务）内省；目录身份与账号身份分属各自服务。

跨服务出站调用统一走 `internal/upstream`（超时分层 + 有界重试 + 熔断；执行器按调用点建一次、长期复用，
连接池与熔断器都在实例里）。参数按调用点收口：

| 调用点 | 上游 | 尝试 | 单次超时 | 总预算 | 退避（指数 + 抖动） | 熔断 |
| --- | --- | --- | --- | --- | --- | --- |
| 实体可见性 `internal/catalog` | `catalog` | 2 | 2s | 5s | 100ms → 500ms，抖动 ±50% | 连续失败 5 次 → 打开 10s，之后放一个半开探测 |
| 会话兜底 `internal/auth`（`GET /api/auth/me`） | `auth` | 2 | 1.5s | 4s | 100ms → 400ms，抖动 ±50% | 同上 |
| PAT 内省（`POST /api/auth/tokens/introspect`） | `auth` | 2 | 1.5s | 4s | 100ms → 400ms，抖动 ±50% | 同上 |

只有超时、连接失败、5xx 与 429 会被重试（幂等读语义，请求体每次尝试重建）；4xx 是上游的**明确回答**，
一次到底。调用方自己取消（父 `ctx`）不算上游故障，也不进熔断计数——否则用户关页面会把上游"熔断"掉。
JWKS 拉取**刻意不走**这套执行器：公钥拉取自带 10 分钟缓存、未知 `kid` 强刷与"刷新失败回落缓存公钥"
的降级顺序，外加一层重试/熔断会改变验签的降级路径。

## HTTP 契约（`/api/storage`）

| 方法 | 路径 | 鉴权 | 说明 |
| --- | --- | --- | --- |
| POST | `/upload/initiate` | 登录 + `upload` | 直传第一步：命中 sha256 即秒传；否则签发预签名地址（分片则返回 upload_id 与每片地址）。同一 sha256 的未完成上传由上传者本人续传 |
| POST | `/upload/complete` | 登录 + `upload`；资产属他人时另需审核者 | 分片合并后**服务端回读对象重算 sha256**，与声明一致才置 complete 并记 `hash_verified`；不一致返回 `hash_mismatch`（409），超限/超时返回 `hash_verify_too_large`（413）/`verify_timeout`（408） |
| PUT | `/upload/stream/{asset_id}` | 登录 + `upload`；资产属他人时另需审核者 | 服务端流式接收（本地对象模式的主要上传方式，也可作为预签名不可用时的兜底）；落盘前流式计算 sha256 与声明比对 |
| POST | `/bind` | 登录 + `upload`；资产属他人时另需审核者 | 绑定到目录实体，带 `binding_role` 用途 |
| DELETE | `/bindings/{id}` | 绑定创建者/上传者/审核者 | 解绑纠错 |
| POST | `/assets/{id}/block` | 审核者 | 禁发：只翻禁发位，不删绑定、不改校验状态，误禁可逆 |
| POST | `/assets/{id}/unblock` | 审核者 | 解禁：原状态即恢复分发资格（仍需满足另两态） |
| GET | `/moderation/blocked` | 审核者 | 禁发资产清单，支持 `limit`（1–500，默认 100）与 `offset` 分页 |
| GET | `/assets` | 审核者 | 全站资产浏览：`limit`（缺省 50、上限 100）、`offset`、`status`（`pending` / `complete`）、`name`（文件名过滤，最多 100 字节）；返回 `assets`、`limit`、`offset`、`has_more` |
| GET | `/bindings` | 审核者 | 全站绑定浏览：同样使用 `limit` / `offset`，返回 `bindings`、`limit`、`offset`、`has_more` |
| GET | `/assets/{id}` | 可读 | 文件元数据 + 绑定列表 |
| GET | `/assets/{id}/content` | 可读（允许匿名） | **原样**内联分发对象内容（不转码）：给目录数据里需要长期引用、能被 `<img>` 直接加载的地址用；`download` 在对象存储模式下只回预签名地址（会过期、Host 是对象存储端点），不能当稳定地址。位图等展示类型走 `inline`，脚本可执行类型走 `attachment`；响应带 `ETag`（内容摘要）并支持 `If-None-Match` → `304` |
| GET | `/entities/{id}/files` | 实体可见 | 「这个介质/轨道/表达上挂了哪些文件」入口 |
| GET | `/download/{asset_id}` | 可读 | 对象存储模式返回预签名下载地址；本地模式直接流式下发 |
| POST | `/verify-hash` | 可读 / 按 asset 校验需登录 | 只给 sha256 = 秒传探测（只认 `hash_verified=true` 的资产）；给 asset_id = 读回对象重算摘要并与声明比对，同样受回读上限约束 |
| GET | `/stats` | 审核者 | 完成态文件数与占用字节、待完成数、禁发数 |

读取可见性的口径只有一条：**上传者本人或审核者直通，其余人只要任一绑定目标实体可见即可读**。
下载、元数据读取与哈希校验共用这一判定，避免同一份文件在不同接口上出现「能下载不能预览」的差异；
`/assets/{id}/content` 也走同一判定（响应只进私有缓存：可见性按请求判定，不能被共享缓存复用）。

### 内容端点的缓存与条件请求

`/assets/{id}/content` 会被浏览器反复加载（目录列表、详情页、`<img>` 重排），因此这一条的响应头
按"可以当图片缓存"来配，取舍全部服务于同一件事：**可见性只能按请求判定**（存储侧不缓存目录结论，
也不直连目录库）。

| 头 | 取值 | 为什么 |
| --- | --- | --- |
| `Cache-Control` | `private, max-age=300` | 只进浏览器私有缓存。**不给 `public`、不挂 CDN 共享缓存**：共享缓存会把一次成功鉴权的响应复用给无权者，等于把可见性烤死在缓存里。300 秒是接受窗口——副本在窗口内直接复用，不会再回到鉴权这一步 |
| `ETag` | 资产 `sha256`（带引号） | 内容寻址让摘要本身就是版本号：一份资产的字节由它的 sha256 唯一确定，换内容等于换资产，不需要额外的版本列 |
| `If-None-Match` | 命中 → `304`（无正文） | 命中的请求既不读对象存储也不传字节；`304` 仍回显 `ETag` 与 `Cache-Control`，但描述"这一份正文长什么样"的头（`Content-Type` / `Content-Length` / `Content-Disposition`）一律不出现 |
| `Content-Type` | 按资产声明的 mime | 声明缺失或退化成 `application/octet-stream` 时按扩展名、再按内容嗅探补齐；一律 `octet-stream` 会让 `<img>` 破图 |
| `Content-Disposition` | 展示类型 `inline`，其余 `attachment` | 位图/音视频/纯文本/PDF 等白名单类型才内联；`text/html`、`image/svg+xml` 这些会被当文档渲染的一律附件（审计 S-3，见 `internal/handler/mime_policy.go`） |
| `X-Content-Type-Options` | `nosniff` | 声明类型与实际内容不符时，不给浏览器"按内容重新判定"的机会 |

条件请求**不是第二条放行路径**：可见性判定先跑，判定不过就是 `404 not_found`（与 `download` 同码同体），
不会因为客户端手上的摘要与内容对得上就回 `304`，`404` 上也不带 `ETag`（否则探测者能靠它比对内容换没换）。
`pending`（内容还没验完）与禁发（`blocked`）同理，一律 `404`，不出内容也不出 `304`。

### 三态门禁：校验完成 / 目录公开 / 允许分发

一份文件能不能分发出去，由三个正交状态共同决定（生产目标 §6，实现见 `internal/handler/governance.go`）：

| 状态 | 存哪 | 含义 |
| --- | --- | --- |
| 校验完成 | `assets.status`（`pending`/`complete`） | 内容是否经服务端验过。只有 `complete` 能分发；`pending` 是"还没验过"，不是"还没公开" |
| 目录公开 | 目录服务（可见性判定） | 任一绑定目标实体对请求者可见。问不到目录服务是 `503`，不是"不可见" |
| 允许分发 | `assets.blocked`（禁发位） | 审核者的独立处置位。**目录公开不等于文件可分发**；禁发只拦分发，不删绑定、不改状态，解禁即恢复 |

禁发用独立列而不用第三个 `status` 值：`status` 只回答"内容是否验过"，`blocked` 只回答"是否允许分发"，
解禁后原状态仍在，不需要"解禁恢复"的分支。全部下载/预览/签名入口（`download`、`content`、秒传命中的复用、
`verify-hash` 的按 asset 校验、实体文件列表）共用这一判定，无权者对被禁发文件一律看到 `404`（不区分无权限与不存在）。

**问不到目录服务 ≠ 不可见**：目录侧明确回答"不存在/不可见"仍是 `404 not_found`；而超时、连接失败、
5xx 或熔断打开时，`/entities/{id}/files`、`/assets/{id}`（及其 content / download / verify-hash 的读取判定）
回 `503` + `{"error":"upstream_unavailable"}`（机器码 `upstream.CodeUpstreamUnavailable`）。
目录地址为空、身份响应缺少 `complete` 或标记 `complete=false` 时也返回同一依赖错误；部分历史别名不能作为全集使用。
把两者折成一个结果，会让上游抖动表现成"这份文件不存在"——文件从实体列表里静默消失，用户与运维都看不出区别。

### 签名撤销窗口

对象存储模式的 `download` 返回预签名地址，有效期 `STORAGE_PRESIGN_TTL_MINUTES`（默认 **120 分钟**）：
签发出的地址在有效期内持续有效，**事后改禁发位/解绑拦不住它**——那是对象存储与签发方的直接约定。
因此不要宣称"已禁发"等于"已阻断一切访问"。要求立即禁发的分发走两条路：调小预签名有效期
（窗口按分钟收敛），或走鉴权代理 `/assets/{id}/content`（每次请求重新鉴权，无残留窗口）。

### 就绪探针

| 探针 | 语义 |
| --- | --- |
| `GET /health` | 进程存活 |
| `GET /ready` | **浅探针**：只探 PostgreSQL（2s 上限，毫秒级返回），编排按固定间隔打它 |
| `GET /ready?deep=1` | **深探针**：在浅探针之上并发探 `CATALOG_URL` 与 `AUTH_URL` 的 `/ready`（总预算 3s），响应体多一项 `"upstreams":[{name,status,breaker,reason,latency_ms}]` |

深探针回答的是"依赖全绿吗"：任一上游不是 `ready` 时就回 `503` 且 `status:"degraded"`，
未配置地址的目标记 `not_configured`（同样计入 degraded，让"没配"与"配了但挂了"都不会被漏看）。
浅探针语义保持不变——上游抖动不该让编排摘掉一个还能正常读写的实例。

### 权限码

授权以账号服务下发的权限码为准（访问令牌的 `permissions` 声明，或 `GET /api/auth/me`），
集中判定在 `internal/auth/permission.go` 的 `Principal.Can`：码优先（`*` 通配即全权），
**缺 `permissions` 键的老令牌仅保留历史上传边界**（上传仍可，审核不再凭 `admin` 角色放行）；
**显式空权限不再回落**，第三方 OAuth 令牌默认拒绝治理码（S01，见 `internal/auth/permission.go`）。

| 码 | 含义 | 当前覆盖 |
| --- | --- | --- |
| `storage.asset.upload` | 上传资源：创建自己的资产、完成直传、绑定用途 | 路由表中的 `upload/initiate`、`upload/complete`、`upload/stream`、`bind` 四个写接口（缺码 `403 forbidden`）；登录本身仍由 401 判定。`unbind`、读接口与 `/stats` 不收此码 |
| `storage.asset.moderate` | 审核资源：完成/接收/绑定/解绑/读取他人的资产、全局统计 | 读表里所有标「审核者」的位置 |

代码里的判定不做角色比较：给某个组授予 `storage.asset.moderate` 即可让成员承担审核（例如建一个
`storage_moderator` 组），不必再改本服务代码。

**个人访问令牌（PAT）**：`Authorization: Bearer mfp_…`（`mfp_` + 43 位 base62）由账号服务的
`POST /api/auth/tokens/introspect` 判定，本服务**不读账号库、不签发、不落盘凭据**。内省拿到的身份与 JWT 同形
（id / username / role / permissions），但**权限一律按 permissions 里的码判定**：即使权限集合为空也绝不回落到
角色兜底或历史的"登录即可"边界——PAT 的权限就是账号服务算好的"用户自身权限 ∩ scopes"，否则 `scopes=[]`
的管理员令牌会变成全权令牌（创建端点已禁止空 scopes，这是第二道防线）。

- 内省结果按明文 sha256 **进程内缓存 60 秒**（同键并发只打一次账号服务，缓存有上限与逐出），
  因此**吊销与过期最长 60 秒后才在本服务生效**；
- 本地先做形态预检（`mfp_` + 43 位 base62），明显非法的明文直接 `401 invalid_token`，不打账号服务；
- 令牌无效 / 已吊销 / 已过期 / 账号被封禁 → `401 invalid_token`（共用一个稳定机器码，不细分原因）；
- 账号服务不可达、内省端点未上线或未配置 `AUTH_URL` → `503 auth_unavailable`，**不是 401**：
  那是依赖故障，回 401 会让 bot/CI 以为凭据有问题去换令牌（换令牌解决不了，重试才行）；
- 状态码映射的边界（别按字面"非 200 都当不认"改回去）：只有 `401` / `403` 是账号服务对**令牌本身**的判定，
  才回 `401 invalid_token`；`503`（账号服务读不动库）、`404`（内省端点还没上线，滚动部署期）、
  `429`（内省限流）与 5xx / 网络超时都**不是**"令牌无效"的证据，一律回 `503 auth_unavailable`——
  照字面把它们也回 401，会让 bot/CI 把有效令牌当废令牌丢掉（换令牌解决不了这些故障，重试才行）；
- PAT 请求不回落 `mf_session` Cookie（浏览器里可能同时有另一个用户的会话），也不产出 Cookie。
  实现与回归见 `internal/auth/pat.go`；三处（目录 / 互动 / 存储）必须同改，口径见主仓库 README 的同名字段。



### 绑定用途（binding_role）

`binding_role` 用字段码表达用途，默认 `master_archive`，取值需匹配 `^[a-z][a-z0-9_]{0,31}$`，
不设封闭枚举：`track_audio`（分轨音频）、`disc_image`（整碟镜像）、`video`、`scans`（扫描件）、
`subtitle`、`ebook` 等由运维与编目约定，新增用途不需要改代码。

`cover_image` 是目录侧自托管封面的预设：一张图一条绑定，`target_entity_id` 就是引用这张图的
目录实体，稳定地址是 `GET /api/storage/assets/{id}/content`，目录侧把它写进
`pictures[].url`、把同一个资产 uuid 写进 `pictures[].asset_id`。展示顺序由目录侧的
`pictures[]` 数组顺序决定，存储侧不另存排序列（`storage.bindings` 没有 position 列）。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `PORT` | `8082` | 监听端口 |
| `TRUSTED_PROXIES` | `127.0.0.1/32,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16` | 可信反向代理的 IP/CIDR 列表（逗号分隔）：只有对端落在列表里时 gin 才采信 `X-Forwarded-For`，否则 `ClientIP()` 回退 `RemoteAddr`。`none` 表示入口链上没有代理（服务被直接暴露）。非法项**拒绝启动**——静默退回"无可信代理"会让审计行的 `actor_ip` 全是网关容器地址、应用层限流退化成全站共享一个桶，而那种退化在功能上看不出异常 |
| `DATABASE_URL` | **必填** | PostgreSQL 连接串，使用存储域独立数据库身份；为空即拒绝启动，不读取 `DB_*` |
| `STORAGE_ROOT` | `./storage-data` | 本地对象模式根目录与直传暂存目录 |
| `STORAGE_S3_ENDPOINT` | 空 | 为空即本地对象模式（无需 RustFS） |
| `STORAGE_S3_PUBLIC_ENDPOINT` | 同内部端点 | 客户端直传使用的对外地址。SigV4 覆盖 Host，必须用浏览器可达的地址签发，否则反代后签名校验失败 |
| `STORAGE_S3_ACCESS_KEY` / `_SECRET_KEY` / `_BUCKET` / `_TLS` | — | 对象存储凭据与桶（旧名 `ARCHIVE_S3_*` 同义） |
| `STORAGE_JWKS_URL` | `http://auth:8081/api/oidc/jwks` | 验签公钥来源：账号服务是唯一签发方 |
| `AUTH_URL` | 空 | 账号服务地址：PAT 内省（`POST /api/auth/tokens/introspect`）及健康探针；留空时 PAT 一律 `503 auth_unavailable` |
| `AUTH_JWT_PUBLIC_KEY` | 空 | 静态公钥（PEM 或 base64 PEM）；设置后不再请求 JWKS |
| `AUTH_JWT_ISSUER` / `AUTH_JWT_AUDIENCE` | `https://findverse.cc/api` / `metafusion` | 与主仓库保持一致，避免存量令牌失效 |
| `CATALOG_URL` | `http://backend:8080` | 目录服务地址（可见性判定） |
| `STORAGE_PRESIGN_TTL_MINUTES` | `120` | 预签名有效期；同时是上传租约的默认寿命（签发地址过期了租约也没意义） |
| `STORAGE_MAX_PARTS` | `10000` | 单次上传最大分片数 |
| `STORAGE_MAX_UPLOAD_MB` | `0` | 服务端接收路径的上限，`0` 为不限制（直传路径不受此限） |
| `STORAGE_VERIFY_MAX_MB` | `0` | complete 阶段回读重算 sha256 的**单对象大小上限**，`0` 为不限制；超限返回 `hash_verify_too_large`，资产留在 pending，不置完成 |
| `STORAGE_VERIFY_TIMEOUT_SECONDS` | `0` | 同一段回读的墙钟上限（秒），`0` 为不限制；超时返回 `verify_timeout`，同样不置完成 |
| `STORAGE_PENDING_TTL_HOURS` | `72` | 上传租约时长（小时）；过期未完成、无绑定的 pending 由 worker 回收 |
| `STORAGE_UPLOAD_LEASE_MINUTES` | 跟随预签名有效期 | 单次上传租约（分钟）：`initiate` 落定、续传刷新；`0`/未配置即跟随 `STORAGE_PRESIGN_TTL_MINUTES` |
| `STORAGE_USER_QUOTA_MB` / `STORAGE_SITE_QUOTA_MB` | `0` | 容量预算（MB）：complete 计真实字节，pending 计声明大小；`0` 为不限制；超限 `initiate` 回 `413 quota_exceeded` |
| `STORAGE_USER_CONCURRENT_UPLOADS` / `STORAGE_SITE_CONCURRENT_UPLOADS` | `0` | 并发预算（进行中的 pending 数）：`0` 为不限制；超限回 `429 too_many_uploads`；只拦新建占位，不拦续传收尾 |
| `STORAGE_ORPHAN_RETENTION_DAYS` | `7` | 孤儿对象保留期（天）：无引用的键首次发现超过该天数才搬隔离区；隔离不等于删除 |
| `STORAGE_S3_SKIP_BUCKET_ENSURE` | `false` | 置 `true` 跳过启动建桶检查，用仅目标桶数据操作的服务凭据（最小权限）；桶由运维身份事先建好 |

服务**刻意不设** `ReadTimeout`/`WriteTimeout`：整盘镜像与视频的上传/下载都可能远超 30 秒，
全局超时会直接截断慢传输（单体当前的 30 秒限制即此原因）。需要收敛时按路由单独加超时。

两个 `STORAGE_VERIFY_*` 默认都不限制，与大文件优先的取向一致；它们的取舍是明确的：

- 预签名直传的内容**没有经过服务端**，`initiate` 采信的 sha256 只是客户端声明，
  因此 `complete` 必须把对象整份读回、重算摘要才能落定——代价随对象线性增长（一次完整读）。
  **对象越大越可能撞上限**：超过上限的对象要显式失败（不会"跳过校验但置 complete"），
  这类大文件应改走服务端流式接收 `PUT /upload/stream/{asset_id}`（边收边算，没有二次读回成本），
  或分片上传并在 `complete` 前自行核对；
- 客户端可以用 `POST /verify-hash`（带 `asset_id`）在收尾前先自检一次，提前拿到 `verified=false`，
  不必等到 `complete` 才失败。

配置了 `STORAGE_S3_ENDPOINT` 时，服务在启动阶段**自己保证桶存在**（`internal/objects` 的 `ensureBucket`：
先 `BucketExists` 再 `MakeBucket`，并发下按"已存在"容忍，幂等）。
桶不再由一次性的 `minio/mc` 初始化容器创建——该镜像已从 Docker Hub 撤下，拉不到会让整条部署链失败；
桶归属服务本身，判定条件与"服务能否连上对象存储"完全一致，因此也不需要额外的启动顺序依赖。

### 对象最小权限

默认（上段）服务凭据需要建桶能力；生产用**仅目标桶的服务凭据**时置 `STORAGE_S3_SKIP_BUCKET_ENSURE=true`：
服务只做目标桶内的数据操作（读写/删/分片/签名），`BucketExists`/`MakeBucket` 一律跳过，
桶由运维的管理身份事先建好（管理身份与服务凭据分离，不共用）。跳过的是"保证桶存在"，
不是"保证能读写"——读写失败仍在首次请求时显式报错，不会静默换存储语义。
服务凭据的最小策略示例（仅目标桶，`<bucket>` 换实际名）：

```json
{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["s3:GetObject", "s3:PutObject", "s3:DeleteObject",
               "s3:AbortMultipartUpload", "s3:ListBucketMultipartUploads", "s3:ListMultipartUploadParts"],
    "Resource": ["arn:aws:s3:::<bucket>", "arn:aws:s3:::<bucket>/*"]
  }]
}
```

主仓编排与角色配置见主仓 [数据库角色](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/database-roles.md) 与 [存储运行约定](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/storage-operations.md)。本服务使用目标桶的 `STORAGE_S3_ACCESS_KEY` / `STORAGE_S3_SECRET_KEY`；桶由运维身份预建时设置 `STORAGE_S3_SKIP_BUCKET_ENSURE=true`。容量、并发、签名时效及回收参数见本 README 的配置和 worker 章节，不另维护 Compose 变量准备稿。

### 完成上传时的内容校验（P1 完整性约束）

内容寻址的前提是"键上的内容确实等于该 sha256"。直传路径上这个前提**不由上传者承诺**，因此：

- `complete` 在合并后回读对象、重算 sha256：一致才置 `complete` 并写 `hash_verified=true`；
  不一致返回 `409 hash_mismatch`，资产留在 `pending`、`fail_reason` 记下原因，**绝不发布**；
  同时删掉那个对不上声明的内容寻址键，避免它继续占位；
- **秒传/查重只复用 `hash_verified=true` 的资产**（`internal/store` 的 `VerifiedAssetByHash`）：
  `initiate`、`verify-hash` 的秒传探测都走这条判据，未验证命中一律按"没有"处理并继续正常上传；
  `CompleteAsset` 本身也只认 `hash_verified=true` 的行，任何新增上传路径都无法把未验内容推成完成态；
- 回读校验的两个上限见上方环境变量；超限/超时都是**可识别的显式失败**，不允许静默降级；
- 失败上传留下的 `pending` 资产仍然占着该 sha256 的唯一索引（内容寻址只登记一份），
  由**上传者本人**重传正确内容即可收尾；同一 sha256 换人上传会被唯一索引挡住。

```bash
# 冒烟：正确内容 → complete 200 且 hash_verified=true
#       等长的错内容 → complete 409 hash_mismatch，资产 pending，该 sha256 秒传不再命中
go test -count=1 ./internal/handler/ ./internal/objects/   # 两种对象模式各覆盖一遍
```

## 运行

直接运行时设置 `DATABASE_URL`，使用主仓授权 SQL 中的 `mf_storage` 身份。数据库实例可共用，业务凭据按域隔离，存储服务仅拥有存储域及所需审计权限；授权规则见主仓 [database-roles.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/database-roles.md)。连接串中的密码须进行 URL 编码。

独立 Compose 部署先将 `.env.example` 复制为 `.env`，设置 `STORAGE_DATABASE_URL`。编排将它注入容器的 `DATABASE_URL`。

```bash
go run cmd/server/main.go          # 需要 DATABASE_URL 指向 PostgreSQL
go test ./... && go vet ./...
```

未配置 `STORAGE_S3_ENDPOINT` 时走本地对象模式：`initiate` 返回 `direct_upload_url`，
客户端 `PUT` 原始字节到该地址即完成入库，服务端边收边算 sha256。

### 后台任务（上传回收与一致性核查）

```bash
storage-server worker   # 跑一次就退出：过期上传回收 + 双向对账，由 cron/systemd timer 周期触发
```

- **上传回收**：按 `upload_expires_at` 租约到期判断；迁移 `000005` 将存量无租约的 pending 行按历史默认 72 小时回填。
  且无绑定的 `pending` 才进候选。处置前重查绑定（候选与删除之间可能有人刚绑上），
  再做双向核对：对象键前缀必须与声明 sha 对上（对不上只上报不动手）；
  仍有其它资产行引用同一键（秒传去重共享）时只删行、不删字节；独占且对象存在才删字节再删行。
- **一致性核查**：库→对象（`complete` 行是否真有字节；缺失的先禁发标记，不解绑不删行）与
  对象→库（无引用的键记 `storage.orphaned_objects` 台账，超过保留期搬 `quarantine/` 隔离，**不自动删字节**）
  双向进行。熔断：读错存在、或缺失多且占比过半（像整桶不可用）时只上报不标记。
- 两条铁律：清理绝不动被绑定的资产；删字节前先隔离标记。"不知道"（读失败、键对不上、比例异常）
  一律只上报不动手。实现见 `internal/maintenance`，口径单测见 `policy_test.go`。

## 数据库结构与迁移

表结构在 `internal/store/migrations/*.up.sql`（基线 `000001_init.up.sql` + 审计表 `000002_audit_log.up.sql`
+ 生命周期 `000003_lifecycle.up.sql`：上传租约列、禁发位、孤儿台账），
由 `internal/store` 在启动时应用（`Init` → `Migrate`）；每个版本一个事务，
DDL 与记账同事务提交，账本表是 `storage.schema_migrations(version, applied_at)`。

- 迁移文件用 `go:embed` 打进二进制：镜像里只有 `/app/storage-server`，文件必须随二进制走；
- 每条语句都幂等（`IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`）：老实例重复启动不改结构，
  历史实例缺列（例如后补的 `fail_reason`）也由同一份文件补齐，不再另写"补丁迁移"；
- 迁移期间取事务级 advisory lock（键 740204，与目录服务的 740202、账号服务的 740203 分开）：
  多副本同时启动时只让一个实例执行 DDL，其余实例等它提交后按账本空转；
- 审计表在**跨服务共用的 `audit` schema**（不属于本服务的领域数据）：迁移文件里在同一事务内
  另取跨服务建表锁 740205，与上面的 740204 是两把锁，不会互相顶掉；
- 账本只记"这一版执行过"，不校验结构本身。手工删过表而账本还在时启动不会重建，
  这种情况删掉对应账本行（`DELETE FROM storage.schema_migrations WHERE version='000001_init'`）再重启。

数据库授权统一使用主仓 `deploy/sql/roles-least-privilege.sql` 与 `deploy/sql/verify-role-isolation.sql`，Compose 通过 `STORAGE_DATABASE_URL` 为服务配置 `mf_storage` 角色；本仓旧角色样例已移除。跨服务角色、共享 `audit` schema 的追加权限与回滚以主仓 [database-roles.md](https://github.com/MoeclubM/MetaFusion/blob/main/docs/architecture/database-roles.md) 及统一 SQL 为准，避免维护第二套角色名称和授权规则。

## 审计留痕

写路由（`POST/PUT/PATCH/DELETE`）成功与失败都落一行到跨服务共用的 `audit.audit_log`
（契约 `docs/architecture/audit-log.md`，本仓库实现在 `internal/audit`）。动作码按路由登记在
`internal/handler/audit.go` 的 `auditActions`：

| 路由 | 动作码 |
| --- | --- |
| `POST /api/storage/upload/initiate` | `asset.upload_initiated` |
| `POST /api/storage/upload/complete` | `asset.upload_completed` |
| `PUT /api/storage/upload/stream/:assetId` | `asset.upload_streamed` |
| `POST /api/storage/bind` | `binding.created` |
| `DELETE /api/storage/bindings/:id` | `binding.removed` |
| `POST /api/storage/assets/:id/block` | `asset.blocked` |
| `POST /api/storage/assets/:id/unblock` | `asset.unblocked` |

- `POST /api/storage/verify-hash` 是**刻意豁免**的写路由：它是读语义的探测与摘要回读校验，
  且探测允许匿名，写审计等于给只读探测开一个刷表入口（理由写在 `auditExempt`，守卫测试要求非空）；
- 写入是**非阻塞旁路**：队列满或落库失败只记日志、丢一行，绝不回滚业务写入；
  流式上传只记元数据（assetId / sha256 / size / mime），不读也绝不落请求体；
- 本服务**没有**审计读取端点：查询面在账号服务的 `GET /api/admin/audit-logs`；
- 新增写端点必须同时登记动作码，否则 `TestWriteRoutesAreAudited`（遍历 gin 路由树）会失败。

## 测试

```bash
go test ./...                 # 全部离线可跑：路由契约、内容寻址键、可见性边界、直传链路
STORAGE_TEST_DSN='postgres://…/metafusion_storage_test' go test ./...   # 追加真实数据库回归
```

其中 `internal/objects/s3_test.go` 用一个只实现必要动作的**假 S3 端点**端到端覆盖了
"建分片会话 → 逐片签发预签名地址 → 客户端 PUT → 合并 → 回读校验"这条链路
（单体原本只做服务端中转上传，这段代码在拆分时才出现，也是最容易只在真实环境暴露问题的地方）。
它已经抓到过一个真实缺陷：分片合并后若采信客户端库返回的 Size（S3 合并响应体里没有长度，该值为 0），
"声明大小 vs 实际大小"的校验会把每一次分片上传都判成 `size_mismatch`——现在改为 HEAD 回读真实大小。

## 迁移状态

- 主仓库的 `/api/archive/*`、`/api/playback/*`、`/api/media/*` **已退役**（网关不再为它们单列 location，相关实现与表已删除）；
  本服务的 `/api/storage/*` 是唯一契约，切流已完成，回退按网关前缀切换。
- 异步转码/HLS/雪碧图投递、BT 种子与磁力链、分片续传的断点记录尚未实现（契约预留）。
