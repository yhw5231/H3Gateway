# 更新说明

本文件记录各版本的变更。格式参考 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

---

## [未发布]

### 修复

- **生成结果错位：新任务拿到旧视频 / 新图不生效。** 上游把上传的首帧图与创建的任务绑在
  提交它们的身份上（`client_id` / `visitorId` / `X-Forwarded-For`），而旧实现成功后会把
  身份放回池子复用第 2 次额度，于是同一个身份的后一次生成会渲染上一次的图，或者被上游
  原样交回上一次的任务——调用方看到「换图没用」「同一个地址的第二次请求直接返回上一次的
  成品」。现在：
  - **一次生成一个身份**：`Rotator` 只铸造不回收（`Mint` / `Retire`），不再有地址池；
  - **幂等键不再取自身份**：`Idempotency-Key` 每个生成独立（重试沿用同一个，避免重复扣额度），
    旧实现直接用 `client_id`，而该值本来就是被复用的；
  - **旧任务拦截**：提交响应里的上游任务号若已经属于别的本地任务，判定上游复读了旧任务，
    退休身份、换幂等键重投；连续 5 次则返回 `502 upstream_replay`，绝不把旧视频当作新结果。

### 变更

- **API 密钥支持「显示」与「复制」**：密钥列表默认只显示前缀，每行新增「显示 / 隐藏」
  与「复制」按钮。明文不再只能在创建时看一眼——后台随时可以查看并一键复制（复制优先
  用 `navigator.clipboard`，在 http + 局域网地址下自动回退到 `execCommand`）。
- `GET /v1/trial/usage` 不再返回 `pool_ready`（地址池已移除）；后台仪表盘改为展示
  「已铸身份 / 退役」与「每次生成独立身份」。

### 新增

- `GET /admin/api/keys/{id}/secret`：按需返回单个密钥的完整明文。需要有效的后台会话，
  每次调用写入 `api key revealed` 审计日志（含客户端 IP）；未知 id 返回 `404`。
  列表接口仍然**不返回明文**，明文只在创建响应与这个显式接口里出现。
- `Store.TaskByUpstreamID`：按上游任务号反查本地任务，用于识别上游的复读行为。

### 验证

- `go build` / `go vet` / `gofmt -l` 干净，`go test ./...` 全部通过
- 回归用例：明文逐字符一致、无会话 `401`、未知 id `404`、列表仍不泄露明文
- 控制台契约测试（选择器、接口路由）随新按钮与新接口自动覆盖
- 真实进程冒烟：登录 → 建密钥 → 列表无明文 → 拉取明文一致 → 无会话 `401` → 未知 id `404`
- 生成错位回归：验证「首帧图绑身份」的假上游下第二次生成渲染的是第二张图；
  上游交回旧任务时三次生成得到三条不同视频；上游持续复读时明确失败且不落库
- **线上双图实测**（`web/studio/examples/ex1.jpg` 与 `ex2.jpg` 连续两次提交，全程无重投）：
  两个本地任务拿到**两个不同的上游任务号**、**两个不同的身份与伪造地址**，
  各自渲染出**不同**的 MP4（1,143,352 字节与 1,260,929 字节，SHA-256 不同）：

  ```
  task submitted task=video_b3ce40f109b33fb7 upstream=2104836270833598464 xff=100.16.17.198 attempt=1
  task submitted task=video_077f6fed523f941b upstream=2104836275321634816 xff=77.113.205.62 attempt=1
  ```
- 顺带修掉一个**不稳定用例**：`TestSessionRejectsTamperingAndWrongSecret` 原先靠改写
  token 最后一个 base64 字符来「篡改」载荷，而该字符的未用位可能解码出完全相同的字节，
  于是断言随机失败（20 次里有 4~6 次）。现在改为解码载荷、翻转末尾字节、再重新编码，
  篡改必然生效，`-count=30` 稳定通过

---

## [0.1.0] - 2026-09-28

**首个 Go 版本：整体重写原 Python/FastAPI 实现。**

本项目是 [HaizhuAI/HaizhuVideo-H3Gateway](https://github.com/HaizhuAI/HaizhuVideo-H3Gateway)
的 Go 重写。原仓库为单文件 Python 实现（`gateway.py`，37 KB）。本次以 Go 标准库重写，
去掉全部第三方依赖，并新增后台管理控制台、API 密钥管理、IPv6 支持与容器部署资产。
旧实现保留在 git 历史中（提交 `ce06ebf`），当前分支不再包含 Python 代码。

### 新增

**后台管理控制台**（`/admin`，默认账号 `admin` / `admin`）

- 六个页面：仪表盘、任务记录、API 密钥、运行设置、XFF / IPv6 测试、账号安全
- 深色主题单页应用，无前端框架依赖（原生 JS）
- 默认密码登录后服务端强制锁定其它接口（`403`），改密后自动换发会话
- 仪表盘展示任务统计、身份轮换计数、在飞请求、上游连通性一键探测
- 任务页支持状态筛选、关键词搜索、分页、详情查看、在线播放/下载、重投、删除

**API 密钥管理**

- 新建 / 启用 / 停用 / 删除 / 备注 / 有效期 / 每分钟限速
- 完整密钥**仅在创建时返回一次**，列表与日志都不出现明文
  （后续版本起可经 `GET /admin/api/keys/{id}/secret` 按需查看，见「未发布」）
- 只要存在**已启用**的密钥，`/v1/*` 自动要求鉴权；显式提供错误密钥一律 `401`，
  不会退回匿名放行

**随机公网 X-Forwarded-For，含 IPv6**

- 五种模式：`off` / `ipv4` / `ipv6` / `mixed` / `auto`
- `auto` 依据实测结论选择；**未确认 IPv6 时安全降级为 `ipv4`**，不盲目伪造
- 两种地址池：`public`（真实公网段）与 `reserved`（RFC 5737 / RFC 3849 文档段）
- 可选文本变体（压缩/展开/大小写随机），因上游按原始字符串计配额
- 配置 `PROXY_LIST` 后**不再伪造 XFF**（真实出口由代理决定），避免误导

**XFF / IPv6 配额键实测**（`/admin` 的测试页，或 `h3gateway probe`）

- 采用**对照键方法**：除对比目标地址提交前后的 `remaining` 外，还检查一个同族
  全新地址是否未被扣减，从而排除「上游忽略请求头、退回按真实出口 IP 计数」的假阳性
- 结论持久化，供 `auto` 模式使用
- `-dry-run` 只做连通性检查，不消耗额度，且**不会给出任何配额键结论**
- **实测结论：上游按 `X-Forwarded-For` 的原始字符串计配额，IPv4 与 IPv6 均可用**；
  IPv6 形式的伪造地址已端到端生成成功

**容器部署**

- 多阶段构建：`golang:1.23-alpine` 构建、`alpine:latest` 运行，镜像约 26 MB
- 以非 root 的 uid/gid `10001` 运行；`/data` 单卷覆盖数据库、视频缓存、上传原图
- `HEALTHCHECK` 走 `/healthz`
- **只使用容器默认 bridge 网络，不创建任何项目网络**
  （`network_mode: bridge`，compose 文件中没有任何 `networks:` 段落）
- `docker-compose.yml` 支持 `H3_PORT` / `H3_IMAGE` / `H3_CONTAINER` / `H3_VOLUME` /
  `H3_VERSION` 覆盖

**非容器部署**

- `deploy/h3gateway.service`：加固过的 systemd 单元
  （`ProtectSystem=strict`、`NoNewPrivileges`、`MemoryDenyWriteExecute` 等，
  唯一可写路径为 `/var/lib/h3gateway`）
- 子命令：`serve`（默认）、`probe`、`version`、`help`
- `serve` 支持 `-host` / `-port` / `-log-level`；`probe` 支持
  `-dry-run` / `-upstream` / `-json`

**OpenAI 风格接口**

- `POST /v1/videos`（JSON + data URL / 图片链接，或 multipart 上传）
- `GET /v1/videos/{id}`、`GET /v1/videos/{id}/content`
- `POST /v1/chat/completions`（兼容 `messages[].content[].image_url`，支持 SSE）
- `GET /v1/models`、`GET /v1/trial/usage`、`GET /health`、`GET /healthz`
- 视频对象同时返回 `video_url` 与 `url`，兼容不同客户端

**持久化与可靠性**

- 单文件 JSON 数据库，原子写入（临时文件 + rename）与 1 秒去抖落盘
- 生成成功后把 MP4 缓存到本地，上游回收任务后仍可下载
- 重启后自动接管未完成任务，不再重投已成功的任务
- 上游失败按错误语义分类处理：限流轮换身份、鉴权门轮换并计数、5xx 指数退避、
  4xx 直接失败

### 变更（相对 Python 版）

- **运行时**：Python 3 + FastAPI + uvicorn + httpx → **纯 Go 标准库，零第三方依赖**
- **存储**：SQLite / 内存 → 单文件 JSON 文档库
- **口令哈希**：第三方库 → 自实现 PBKDF2-HMAC-SHA256（210,000 次迭代，32 字节随机盐）
- **默认通道**：`showcase` → `plain`。上游已给 showcase 通道加上登录墙
  （`401 login_required`），匿名不可用；`plain` 是当前唯一可用通道
- **网页工作台**：资源路径由 `/` 改写为 `/studio/`，并在顶栏加入后台入口
- **配置文件**：`.env.example` 全部重写并加中文注释；新增
  `GATEWAY_XFF_MODE` / `GATEWAY_XFF_POOL` / `GATEWAY_XFF_VARIANTS` /
  `GATEWAY_RESUBMIT_BACKOFF` 等变量
- 新增 `.env.example` 变量名与代码的自动漂移检查（`TestEnvExampleOnlyUsesKnownVariables`）

### 修复

重写过程中发现并修复的缺陷（均附带回归测试）：

- **成功后身份被双重递减**：上游返回的 `remaining` 已赋值给 `UsesLeft`，代码又额外
  递减一次，导致每个伪造地址只用 1 次就被退休，**有效容量减半**。改为以上游返回值
  为准，仅在字段缺失时才本地递减
- **后台静态资源返回 HTML 外壳**：`/admin/admin.js` 与 `/admin/admin.css` 落到
  `/admin/` 子树通配，返回 `index.html` 且状态码为 200，浏览器拒绝执行，
  **整个控制台不可用**。改为单段路由 `GET /admin/{asset}` 并按扩展名设置
  `Content-Type`，缺失资源返回真正的 `404`
- **账号删除后会话仍有效**：会话是无状态签名令牌，删除管理员无法吊销。现校验
  账号存在性，并新增 `Store.DeleteUser`
- **`Submit` 在 `Start` 之前调用会 panic**（基础 context 为 nil）
- **dry-run 实测会声称 IPv6 可用**：未提交任何请求却给出结论。现明确不置位
- **未知 `/admin/api/*` 返回 200**（返回 SPA 页面而非 404）
- **设置接口静默钳制越界值**：新增 `ValidateInput()`，越界返回 `400`；持久化加载
  路径仍走归一化修复
- **`/v1/trial/usage` 泄露内部运维计数**：收敛为只返回容量与生效模式
- **`chat/completions` 只取最后一条消息**：末条是 assistant 时无法取图，改为回溯
  最后一条 user 消息
- **后台「重置」只清空地址池不清计数**
- **仪表盘读错字段名**（`tasks.keys` → `tasks.api_keys`），密钥总数永远显示 0
- **dry-run 结果被渲染成「结论：未通过」**，误导运维

### 文档

- `README.md`：特性、四种部署方式、容器网络约束、后台管理、XFF/IPv6 原理与实测
  结论、完整 API 文档、配置项表、目录结构、常见问题、与 Python 版的差异
- `docs/上游接口分析.md`：上游端点清单、配额键证据、IPv6 实测方法与输出、
  multipart `Content-Type` 要求、showcase 登录墙、时长映射与错误语义
- `docs/部署与验证.md`：构建与测试结果、容器网络约束验证、接口冒烟测试、
  后台流程、线上端到端（IPv4/IPv6）、14 项缺陷记录、复现步骤与已知限制

### 验证

- `go build` / `go vet` / `gofmt -l` 全部干净
- **152 个测试全部通过**（`internal/auth`、`config`、`identity`、`pipeline`、
  `server`、`store`、`upstream`、`xffprobe`）
- 容器网络约束实测：完整 `down` → `up` 循环前后 `docker network ls` 列表逐字节一致
- 线上端到端：multipart 提交 → 轮询至 `succeeded` → 下载 1,067,764 字节合法 MP4
  （文件头 `ftypisom`）
- IPv6 端到端：伪造随机公网 IPv6（`2476:27b2:b8fd:1db4:bffb:3847:c12a:e502`）生成成功
- 同一伪造地址连续产出两条视频，用满 2 次额度后才退休
- 后台控制台契约：63 个 DOM 选择器、18 个接口调用、19 个设置字段全部命中，
  并新增自动化守卫（用变异测试验证守卫确实会失败）

### 已知限制

- **`prompt` 是否生效未证实**：上游前端在该通道不发送 `prompt`，网关会转发该字段，
  但上游是否使用无法确认
- **仅支持 9:16 竖屏**：上游试用通道的硬限制；不支持的取值返回 `400`，不静默改写
- **`showcase` 通道不可用**：已被上游登录墙拦截，代码路径保留但默认不启用
- **代理与 XFF 伪造互斥**：配置代理后配额按代理出口 IP 计算
- **Docker Desktop for Windows/macOS 上容器无法访问宿主机 `0.0.0.0` 服务**：
  需要代理时请用 `host.docker.internal`
- **未执行 `go test -race`**：需要 cgo，本次验证环境无 C 编译器
- **额度取决于上游策略**：上游可能随时调整配额规则、增加校验或加登录墙

---

## 历史版本

`0.1.0` 之前的版本为 Python 实现，见提交 `ce06ebf`
（*MiniMax H3 video gateway: OpenAI-compatible API + showcase-channel prompt support*）。
