# H3Gateway（Go 重写版）

把 MiniMax-H3 匿名试用通道包装成 **OpenAI 风格视频接口** 的网关。本项目是
[HaizhuAI/HaizhuVideo-H3Gateway](https://github.com/HaizhuAI/HaizhuVideo-H3Gateway)
（Python/FastAPI 实现）的 **Go 重写**：只依赖标准库，单文件二进制，面向容器部署。

> 原 Python 实现（`gateway.py`）已从当前分支移除，仍可在 git 历史（提交 `ce06ebf`）
> 与上游仓库中查看。各版本变更见 [更新说明](CHANGELOG.md)。

---

## 目录

- [特性](#特性)
- [快速开始](#快速开始)
- [容器网络说明（重要）](#容器网络说明重要)
- [后台管理](#后台管理)
- [随机公网 XFF 与 IPv6 支持](#随机公网-xff-与-ipv6-支持)
- [API 文档](#api-文档)
- [配置项](#配置项)
- [目录结构](#目录结构)
- [常见问题](#常见问题)
- [与 Python 原版的差异](#与-python-原版的差异)

---

## 特性

| 能力 | 说明 |
| --- | --- |
| OpenAI 风格接口 | `POST /v1/videos`、`GET /v1/videos/{id}`、`GET /v1/videos/{id}/content`，另有 `POST /v1/chat/completions` |
| 随机公网 XFF | 每个请求伪造一个随机 `X-Forwarded-For`，上游按该值独立计配额，**IPv4 与 IPv6 均已实测可用** |
| 后台管理台 | 深色主题单页控制台：仪表盘、任务、密钥、设置、IPv6 实测、账号安全 |
| API 密钥管理 | 新建/启停/删除/限速/有效期，列表默认只显示前缀，可随时「显示」明文或一键「复制」 |
| 持久化 | 单文件 JSON 数据库（原子写入），任务、密钥、设置、实测结论全部落盘 |
| 容器友好 | 多阶段构建，非 root 运行，单一数据卷，**只使用默认 bridge 网络** |
| 零第三方依赖 | 纯 Go 标准库，PBKDF2、SOCKS5、会话签名均为自实现，可完全离线构建 |
| 健壮性 | 上游失败自动重投、一次性身份、重复任务拦截、并发闸门、视频本地缓存、重启续跑 |

---

## 快速开始

从拉取仓库开始，四步跑起来；完整流程（初始化、反向代理、升级、备份、排错）见
[部署指南](docs/部署指南.md)。

### 第 0 步：拉取仓库

```bash
git clone https://github.com/yhw5231/H3Gateway.git
cd H3Gateway
```

以下命令都在仓库根目录执行。没有 git 时可下载源码包
（<https://github.com/yhw5231/H3Gateway/archive/refs/heads/main.tar.gz>，解压后目录名为
`H3Gateway-main`）。

### 方式一：Docker Compose（推荐）

```bash
cp .env.example .env      # 按需修改；不改也能直接跑
docker compose up -d --build
```

打开 <http://127.0.0.1:8787/admin>，用 **admin / admin** 登录。

> 首次登录后控制台会被锁定，必须先修改密码才能使用其它功能（见
> [后台管理](#后台管理)）。

常用命令：

```bash
docker compose logs -f          # 查看日志
docker compose restart          # 重启
docker compose down             # 停止（数据卷保留）
docker compose down -v          # 停止并删除数据卷（清空全部状态）
```

### 方式二：docker run

```bash
docker build -t h3gateway:latest .

docker run -d --name h3gateway \
  --network bridge \
  -p 8787:8787 \
  -e GATEWAY_ADMIN_PASSWORD=admin \
  -v h3gateway-data:/data \
  --restart unless-stopped \
  h3gateway:latest
```

### 方式三：本地二进制

```bash
go build -o h3gateway .
./h3gateway serve                       # 默认 127.0.0.1:8787
./h3gateway serve -host 0.0.0.0 -port 9000
```

### 方式四：systemd（Linux 非容器部署）

仓库提供加固过的单元文件 [`deploy/h3gateway.service`](deploy/h3gateway.service)：

```bash
sudo useradd --system --home /var/lib/h3gateway --shell /usr/sbin/nologin h3gateway
sudo install -m 0755 h3gateway /usr/local/bin/h3gateway
sudo install -d -o h3gateway -g h3gateway -m 0750 /var/lib/h3gateway
sudo install -d -m 0755 /etc/h3gateway
sudo install -m 0640 -o root -g h3gateway .env.example /etc/h3gateway/h3gateway.env
sudo install -m 0644 deploy/h3gateway.service /etc/systemd/system/h3gateway.service
sudo systemctl daemon-reload && sudo systemctl enable --now h3gateway
```

该单元默认只监听 `127.0.0.1`（要对外提供服务请在
`/etc/h3gateway/h3gateway.env` 里设 `GATEWAY_HOST=0.0.0.0`，并确认前面有反向代理
或防火墙），并启用 `ProtectSystem=strict`、`NoNewPrivileges`、`MemoryDenyWriteExecute`
等加固项，唯一可写路径是 `/var/lib/h3gateway`。

### 自检

```bash
./h3gateway version                     # 版本
./h3gateway probe                       # 直连上游，实测 IPv4/IPv6 配额键（消耗额度）
./h3gateway probe -dry-run              # 仅检查连通性，不消耗额度
./h3gateway help                        # 用法
```

`probe` 会把结论写入数据文件，后台的「自动」模式会据此选择 IPv4 / IPv6 / 混合。

### 部署注意事项

| 事项 | 说明 |
| --- | --- |
| **数据卷权限** | 容器以非 root 的 uid/gid `10001` 运行。使用**命名卷**时 Docker 会自动沿用镜像里 `/data` 的属主，无需干预；改用**绑定挂载**（`-v /host/data:/data`）时，宿主机目录必须先 `chown 10001:10001`，否则启动后无法写入任务与视频 |
| **端口** | 容器内固定监听 `8787`，`docker-compose.yml` 的 `environment:` 里已硬编码 `GATEWAY_PORT=8787`，且镜像的 `HEALTHCHECK` 也探测该端口。改对外端口请只改 `H3_PORT`（compose）或 `-p`（docker run），不要改容器内的端口 |
| **反向代理** | 在 nginx/Caddy 后面时建议设置 `GATEWAY_PUBLIC_URL`，让返回的下载链接指向对外地址；同时设 `GATEWAY_TRUST_PROXY=true`，使控制台记录的客户端 IP 取自 `X-Forwarded-For` |
| **升级** | 重新构建镜像并 `docker compose up -d`（或替换二进制后 `systemctl restart h3gateway`）。数据全部在数据卷/数据目录，升级不丢任务、密钥与设置 |
| **时区** | compose 默认 `TZ=Asia/Shanghai`，需要别的时区改这个变量 |
| **离线构建** | 无第三方依赖，`docker build` 不需要联网拉包；构建镜像 `golang:1.23-alpine` 与 `alpine:latest` 需事先存在或可拉取 |
| **架构** | 交叉构建请用 `docker buildx build --platform linux/arm64 ...`（BuildKit 会据此设置 `TARGETARCH`）。直接 `--build-arg TARGETARCH=arm64` 只会把 arm64 二进制塞进 amd64 镜像，无法运行 |

---

## 容器网络说明（重要）

**本项目只使用 Docker 默认 bridge 网络，不会创建任何项目网络。**

具体做法：`docker-compose.yml` 里的服务声明了 `network_mode: bridge`，并且
**整个文件没有任何 `networks:` 段落**。因此 `docker compose up` 不会创建
`h3gateway_default` 之类的网络。

验证方法：

```bash
docker network ls                       # 记录当前网络列表
docker compose up -d
docker network ls                       # 列表应与之前完全一致
docker inspect h3gateway --format '{{.HostConfig.NetworkMode}}'   # 输出 bridge
```

实测结果（本机验证记录）：

```
启动前: bridge host ipv6-net localcline_default none novelgenerationgo_default ...
启动后: bridge host ipv6-net localcline_default none novelgenerationgo_default ...   ← 完全一致
容器网络模式: bridge
```

如果你确实需要和其它容器互通，有两种不创建项目网络的做法：

1. 让对端容器也加入默认 bridge：`docker run --network bridge ...`
2. 用 `network_mode: "container:<对端容器名>"` 共享网络栈

> 在 **Docker Desktop for Windows/macOS** 上，容器**无法**通过 `172.17.0.1`
> 访问宿主机上监听 `0.0.0.0` 的服务（容器跑在虚拟机里）。如果上游需要经代理访问，
> 请把代理地址写成 `host.docker.internal:<端口>`（Docker Desktop 提供该名称解析），
> 或改用 `network_mode: "container:<代理容器名>"`。

---

## 后台管理

地址：<http://127.0.0.1:8787/admin>　默认账号：**admin / admin**

### 首次登录必须改密

用默认密码登录后，服务端会拒绝除「修改密码」和「会话查询」以外的所有后台接口
（返回 `403`），控制台顶部同时显示红色提示。改密成功后会自动换发新的会话
Cookie，无需重新登录。

播种逻辑：只有当数据文件里还不存在该账号时，才用 `GATEWAY_ADMIN_USER` /
`GATEWAY_ADMIN_PASSWORD` 创建；`MustChangePassword` 仅在密码等于 `admin` 时置位。
也就是说**改过密码之后，环境变量里的默认值不会再把它改回来**。

### 六个页面

| 页面 | 功能 |
| --- | --- |
| **仪表盘** | 任务总数/成功/进行中/失败、密钥数、身份铸造/退休计数、在飞请求；上游连通性一键探测；最近任务；XFF 当前配置与实测状态 |
| **任务** | 按状态筛选 + 关键词搜索 + 分页；查看详情（含伪造 XFF、上游任务号、access_token、缓存情况）、在线播放/下载 MP4、重投、删除 |
| **API 密钥** | 新建（名称/备注/有效期/限速）、启用停用、删除。列表默认只显示前缀，每行可点「显示」查看完整明文、点「复制」直接复制（明文按需单独拉取，且记录操作日志） |
| **运行设置** | 上游地址、接口通道、默认提示词、XFF 模式与地址池、文本变体、并发、超时、轮询间隔、重投次数与退避、图片体积上限、保留条数、代理列表、访问控制 |
| **XFF / IPv6 测试** | 一键实测「上游是否把 X-Forwarded-For 当作独立配额键」，并分别给出 IPv4 / IPv6 结论；通过后可直接一键切换到 IPv6 或混合模式 |
| **账号安全** | 修改密码；查看监听地址、数据目录与数据文件路径 |

### 访问控制规则

`/v1/*` 的鉴权顺序：

1. 请求头里带了有效的 `Authorization: Bearer <API 密钥>` → 通过
2. 带了工作台 Cookie（`GATEWAY_STUDIO_SESSION=true` 时）→ 通过
3. 带了后台 Cookie → 通过
4. 以上都没有，且**存在已启用的密钥**或开启了 `GATEWAY_REQUIRE_API_KEY` → `401`
5. 以上都没有，且没有任何启用的密钥、也没开启强制校验 → 放行（**默认开放**）

> ⚠️ 默认是开放的：只要能访问端口就能调用。生产环境请在后台新建密钥并在
> 「运行设置 → 访问控制」中开启强制校验。服务启动时若检测到开放状态，日志里会有
> `WARN` 提示。

**显式提供了错误的密钥永远返回 401**，不会退回匿名放行。

### 安全细节

- 密码使用 PBKDF2-HMAC-SHA256，210000 次迭代，32 字节随机盐，十六进制存储
- 会话 Cookie 是 HMAC-SHA256 签名的无状态令牌；`h3_admin` 有效期 12 小时，
  `h3_studio` 30 天；两者均为 `HttpOnly` + `SameSite=Lax`
- 所有会改变状态的后台请求都必须携带 `X-Requested-With` 头并同源，用于阻断 CSRF
- 任务列表不返回密钥明文；密钥列表只返回前缀，完整明文只在创建响应与显式的
  `GET /admin/api/keys/{id}/secret`（需后台会话，并写入 `api key revealed` 日志）中出现；
  日志不打印密钥与 access_token

---

## 随机公网 XFF 与 IPv6 支持

### 原理

上游的匿名试用额度**不是按真实来源 IP 计的**，而是按请求头 `X-Forwarded-For`
的**第一个值的原始字符串**计：

- 每个不同的字符串值，每天 2 次生成
- 比较时**不做任何 IP 规范化**：展开写法与压缩写法算两个桶，
  大小写不同算两个桶，`[2001:db8::1]` 带方括号也算另一个桶
- 私网/保留地址同样被接受，没有任何校验

所以只要每次请求换一个随机地址，额度就近乎无限。

> **每次生成都用全新身份。** 上游不只用 `X-Forwarded-For` 计配额，还会把
> **上传的首帧图**和**创建的任务**绑在提交它们的那个身份上（`client_id` /
> `visitorId` / XFF）。因此复用同一个伪造地址会让后一次生成拿到前一次的图或
> 前一次的成品——表现为「换图没用」「同一个地址的第二次请求直接返回上一次的视频」。
> 地址空间近乎无限，复用毫无收益，所以网关**一个身份只服务一次生成**，用完即退休。

### IPv6 实测结论

**上游完全支持 IPv6 形式的 XFF，与 IPv4 等价。** 这不是推测，而是用
「对照键」方法实测出来的。

测试方法（后台「XFF / IPv6 测试」页或 `h3gateway probe` 执行）：

1. 读一次目标地址的 `/usage`，记录基线 `remaining`
2. 用该地址提交一次真实生成
3. 再读一次该地址的 `/usage`，确认 `remaining` 减少
4. **同时读一个同族但全新未使用的「对照地址」**，确认它的 `remaining` 仍是满额

第 4 步是关键：如果上游忽略了请求头、退回按真实出口 IP 计数，那么对照地址会落到
同一个桶里、同样被扣减。只有「目标地址被扣减 + 对照地址未被动过」才能证明配额确实
由请求头决定。只用「前后对比」的方法会产生假阳性。

本机对线上上游的实测输出：

```
GET https://siftq.com/api/minimax-trial/usage 可达
IPv4 键 198.51.100.153 初始 remaining=2
IPv6 键 2001:db8:d671:abe8:11d3:b296:25a7:dfb2 初始 remaining=2
IPV4 提交成功（键 198.51.100.153），重新读取配额…
IPV4 提交后 remaining 2 -> 1
IPV4 对照键 203.0.113.33 remaining=2/2（未被扣减，说明配额按 XFF 计）
IPV4 结论：✅ 上游以 X-Forwarded-For 作为独立配额键
IPV6 提交成功（键 2001:db8:d671:abe8:11d3:b296:25a7:dfb2），重新读取配额…
IPV6 提交后 remaining 2 -> 1
IPV6 对照键 2001:db8:252d:f441:bb8f:9202:7400:4efc remaining=2/2（未被扣减，说明配额按 XFF 计）
IPV6 结论：✅ 上游以 X-Forwarded-For 作为独立配额键
```

后台的 IPv6 选项（「运行设置 → XFF 模式 → 随机公网 IPv6」）只有在实测通过后才会
被推荐启用；`auto` 模式也会依据实测结论自动升级为 `mixed`。

### 五种模式

| 模式 | 行为 |
| --- | --- |
| `off` | 不伪造，所有请求共用同一个配额桶 |
| `ipv4` | 每次使用随机公网 IPv4 |
| `ipv6` | 每次使用随机公网 IPv6 |
| `mixed` | 随机混合使用 IPv4 / IPv6 |
| `auto` | 按后台实测结论自动选择；**未测试时安全退化为 `ipv4`** |

安全设计：如果配置了 `ipv6` 但实测并未确认 IPv6 可用，网关会自动**降级**为 `ipv4`
而不是盲目伪造，避免整条链路失败。

### 地址池

| 池 | 说明 |
| --- | --- |
| `public`（默认） | 真实公网地址段，地址空间最大（IPv6 约 2^61），与原 Python 版行为一致 |
| `reserved` | RFC 5737（`198.51.100.0/24` 等）与 RFC 3849（`2001:db8::/32`）文档地址段。这些段不会指向任何真实主机，若伪造的地址被第三方日志采集或反向探测，不会牵连无关的真实站点，**更安全但会与其它使用者共享同一个文档段配额桶** |

### 文本变体

`xff_variants` 开启后，会在同一地址的多种写法（压缩 / 展开 / 大写）之间随机选择。
由于上游按原始字符串计数，这等于把每个地址变成多个配额桶。

默认**关闭**：随机 IPv6 本身已经提供约 2^64 量级的地址空间，再叠加变体没有实际
收益，只会让日志更难读。

### 代理与伪造互斥

一旦配置了 `PROXY_LIST`，网关**不再伪造** `X-Forwarded-For`（真实出口地址由代理
决定），配额将按代理出口 IP 计算。这是有意为之：伪造的头会被代理覆盖，留着只会
造成误解。

同理，「XFF / IPv6 测试」**始终直连上游**（不使用代理），否则测不出任何东西。
测试步骤里会明确提示这一点。

> 注意：代理地址不可达时，提交会持续重试直到 `submit_timeout_sec` 超时。配置代理
> 后请先用仪表盘的「探测上游」确认可用。

---

## API 文档

所有 `/v1/*` 接口都遵循 OpenAI 风格：鉴权用 `Authorization: Bearer <API 密钥>`，
错误体为 `{"error": {"message": ..., "type": ..., "code": ...}}`。

### 创建视频

`POST /v1/videos`

支持两种提交方式。

**JSON + data URL / 图片链接：**

```bash
curl -X POST http://127.0.0.1:8787/v1/videos \
  -H "Authorization: Bearer h3-xxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{
        "model": "minimax-h3",
        "image": "data:image/jpeg;base64,<BASE64>",
        "prompt": "镜头缓慢推进，人物轻微眨眼",
        "duration": 6
      }'
```

**multipart 上传：**

```bash
curl -X POST http://127.0.0.1:8787/v1/videos \
  -H "Authorization: Bearer h3-xxxxxxxx" \
  -F "image=@photo.jpg;type=image/jpeg" \
  -F "prompt=镜头缓慢推进" \
  -F "duration=10"
```

参数：

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `image` | ✅ | 图片本体（multipart 的 `image` 字段），或 JSON 里的 `data:image/...;base64,...` / `http(s)` 链接 |
| `model` | | 默认 `minimax-h3`。模型名以 `-10s` / `-15s` 结尾可指定时长 |
| `prompt` | | 提示词。留空时套用设置里的 `default_prompt`（后台「设置」页可改）。会转发给上游，但**该通道的上游前端本身不发送 prompt**；通道实测渲染的是你给的那张首帧图（见 [`docs/上游接口分析.md`](docs/上游接口分析.md) 第 9 节） |
| `duration` / `seconds` | | 4-7 → 6 秒，8-12 → 10 秒，13-20 → 15 秒；默认 6 秒 |
| `ratio` / `size` | | 仅支持竖屏 9:16（上游硬限制）。接受 `9:16`、`720x1280`、`1080x1920`、`vertical`、`portrait` |

返回 `202 Accepted`：

```json
{
  "id": "video_8e59f8f949f80449",
  "object": "video",
  "model": "minimax-h3",
  "status": "queued",
  "progress": 10,
  "created": 1790567763,
  "completed_at": null,
  "ratio": "9:16",
  "size": "9:16",
  "seconds": "6",
  "failure_reason": null,
  "attempts": 0,
  "created_at": "2026-09-28T03:56:03Z",
  "updated_at": "2026-09-28T03:56:03Z"
}
```

### 查询任务

`GET /v1/videos/{id}`

`status` 取值：`queued` → `running` → `succeeded` / `failed` / `canceled`，
`progress` 依次为 10 / 50 / 100。成功后额外返回 `video_url` 与 `url`。

任务还带两个自检字段，用来判断「拿到的是不是我自己这张图的成片」：

| 字段 | 含义 |
| --- | --- |
| `video_sha256` | 成片的 SHA-256。同一个输入图重跑一般不同；两次提交拿到**相同**的值说明上游复用了同一条内容 |
| `duplicate_of` | 若该成片的字节与更早的任务完全相同、而**输入图不同**，则指向最早产出该内容的任务 id，同时写 `WARN` 日志。正常情况下不出现 |

### 下载视频

`GET /v1/videos/{id}/content`

返回 `video/mp4`。文件在生成成功后立即缓存到本地（`<数据目录>/videos/<id>.mp4`），
因此即使上游把任务回收了也仍可下载。

### 试用量信息

`GET /v1/trial/usage` —— 返回当前容量与生效模式（不含任何运维计数）：

```json
{
  "enabled": true,
  "in_flight": 0,
  "max_concurrent": 4,
  "xff_mode": "auto",
  "effective_xff_mode": "mixed",
  "ipv6_supported": true,
  "endpoint_mode": "plain"
}
```

### 模型列表

`GET /v1/models` —— OpenAI 风格的模型清单。

### Chat Completions

`POST /v1/chat/completions`

兼容把图片放在 `messages[].content[].image_url` 里的调用方式（`image_url` 既支持
`{"url": "..."}` 对象形式，也支持裸字符串），返回一条包含任务信息的 assistant
消息；`"stream": true` 时返回 SSE 流。

### 健康检查

`GET /health`、`GET /healthz` —— 无需鉴权，返回 `{"status":"ok","time":"..."}`。

### 其它入口

| 路径 | 说明 |
| --- | --- |
| `/` | 网页工作台（原 Python 版的 `web/`，已改写资源路径） |
| `/studio/*` | 工作台的静态资源 |
| `/admin` | 后台管理控制台 |

---

## 配置项

环境变量只决定**首次启动**（数据文件尚不存在）时的初始值；之后以数据文件里的设置
为准，可在后台「运行设置」里随时修改。

完整清单见 [`.env.example`](.env.example)。常用项：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GATEWAY_HOST` | `127.0.0.1`（容器内为 `0.0.0.0`） | 监听地址 |
| `GATEWAY_PORT` | `8787` | 监听端口 |
| `GATEWAY_DATA_DIR` | `./data`（容器内 `/data`） | 数据目录 |
| `GATEWAY_ADMIN_USER` / `GATEWAY_ADMIN_PASSWORD` | `admin` / `admin` | 首次播种的后台账号 |
| `GATEWAY_SESSION_SECRET` | 随机生成并落盘 | 会话签名密钥 |
| `GATEWAY_API_KEY` | 空 | 首次启动时导入一个 API 密钥 |
| `GATEWAY_UPSTREAM` | `https://siftq.com` | 上游地址 |
| `GATEWAY_ENDPOINT_MODE` | `plain` | `plain` 或 `showcase` |
| `GATEWAY_XFF_MODE` | `auto` | `off` / `ipv4` / `ipv6` / `mixed` / `auto` |
| `GATEWAY_XFF_POOL` | `public` | `public` 或 `reserved` |
| `GATEWAY_XFF_VARIANTS` | `false` | 是否随机改写地址文本写法 |
| `GATEWAY_MAX_CONCURRENT` | `4` | 同时进行的生成数 |
| `GATEWAY_SUBMIT_TIMEOUT` | `900` | 单任务总超时（秒） |
| `GATEWAY_POLL_INTERVAL` | `3` | 轮询间隔（秒） |
| `GATEWAY_TASK_RESUBMITS` | `8` | 上游判失败后的最大重投次数 |
| `GATEWAY_RESUBMIT_BACKOFF` | `30` | 重投基础退避（秒），随次数递增，上限 5 倍 |
| `GATEWAY_IMAGE_MAX_BYTES` | `20971520` | 输入图片上限 |
| `GATEWAY_TASK_RETENTION` | `2000` | 本地保留的历史任务条数 |
| `PROXY_LIST` | 空 | 逗号分隔的代理列表，支持 `socks5://` `http://` `https://` |
| `GATEWAY_REQUIRE_API_KEY` | `false` | 是否强制 `/v1/*` 校验密钥 |
| `GATEWAY_STUDIO_SESSION` | `true` | 工作台 Cookie 能否直接调用 `/v1/*` |
| `GATEWAY_TRUST_PROXY` | `false` | 是否信任 `X-Forwarded-For` 记录客户端 IP |
| `GATEWAY_PUBLIC_URL` | 空 | 对外地址，用于拼装下载链接 |

> `.env.example` 里的变量名有自动化测试守护（`TestEnvExampleOnlyUsesKnownVariables`），
> 代码里改了名字而文档没跟着改，测试会失败。

---

## 目录结构

```
.
├── main.go                    入口：serve / probe / version / help，内嵌 web 资源
├── go.mod                     模块声明（无任何第三方依赖）
├── Dockerfile                 多阶段构建，非 root 运行
├── docker-compose.yml         默认 bridge 网络，无 networks 段落
├── .dockerignore
├── .gitignore
├── .env.example               全部环境变量与中文说明
├── internal/
│   ├── model/                 任务、密钥、管理员、实测记录的数据结构
│   ├── config/                环境变量、运行时设置、归一化与校验
│   ├── auth/                  PBKDF2 口令、API 密钥、HMAC 会话
│   ├── store/                 单文件 JSON 数据库（原子写入 + 去抖落盘）
│   ├── identity/              随机 IPv4/IPv6 生成、一次性身份铸造与退休
│   ├── upstream/              上游客户端：提交、轮询、取片、配额、SOCKS5
│   ├── pipeline/              任务编排：并发闸门、轮询、重投、视频缓存
│   ├── xffprobe/              XFF / IPv6 配额键实测（含对照键方法）
│   └── server/                HTTP 层：路由、鉴权、/v1 接口、后台接口
├── web/
│   ├── studio/                网页工作台（来自原版，资源路径已改写）
│   └── admin/                 后台控制台（index.html / admin.css / admin.js）
├── deploy/                    systemd 单元等非容器部署资产
├── docs/                      上游接口分析、部署指南、部署与验证记录
├── CHANGELOG.md               更新说明
└── README.md                  本文件
```

---

## 常见问题

**Q：后台登录不了 / 一直提示要改密码？**
默认密码是 `admin`/`admin`。用默认密码登录后必须先改密码（至少 8 位），改完会自动
换发会话，其它接口立刻可用。忘记密码时，删除数据文件里的 `users` 字段（或整个
`h3gateway.json`）后重启，会重新按环境变量播种。

**Q：`/v1/*` 返回 401，但我没配密钥？**
说明数据里已经存在**已启用**的密钥，此时接口会自动要求鉴权。到后台「API 密钥」
页面新建一个密钥，或用 `Authorization: Bearer <密钥>` 调用。

**Q：提交一直卡在 `queued` / `running`？**
上游排队时间可能长达十几分钟。任务在后台是异步的，可以随时关闭客户端，稍后
用 `GET /v1/videos/{id}` 查结果。超过 `submit_timeout_sec` 会被判失败。

**Q：视频下载 404？**
只有 `succeeded` 的任务才有内容。失败任务请查看 `failure_reason`。

**Q：出来的视频和输入的图不对应？再点一次「开拍」拿到的是上一段视频？**
这是上游的「身份绑定」行为：它把**首帧图**和**创建的任务**绑在提交它们的身份
（`X-Forwarded-For` + `client_id`）上，复用身份就会渲染旧图、或被直接交回旧任务。
网关的应对是**一次生成一个身份**，并且在校验到上游交回旧任务时退休身份换新重投，
连续失败会返回 `502 upstream_replay` 而不会把旧视频当新结果返回。请确认：
`GET /v1/videos/{id}` 里的 `id` 是**这次** `POST /v1/videos` 返回的那个（旧 id 当然下载到旧视频），
并核对两个任务的 `upstream_task_id` 是否不同——相同即命中了上面这条，升级到本版本即可。

**Q：为什么每条视频里都出现一个女人，结尾还都是一个「飞吻」动作？**
分三层排查（实测数据见 [`docs/上游接口分析.md`](docs/上游接口分析.md) 第 9 节）：

1. **同一条片子被复用**。上游可能换个任务号把同一条内容再发一次，此时看
   `GET /v1/videos/{id}` 的 `video_sha256`（两次提交值相同即为同一条片子）；
   若输入图不同却拿到相同字节，任务上会出现 `duplicate_of`，日志里也有对应的 `WARN`。
   这一层现在可观测；任务号级的复读则直接返回 `502 upstream_replay`，不会当新结果返回。
2. **输入图本身就是那样**。匿名试用通道渲染的就是你给的那张首帧图——实测把一张合成的
   纯绿几何图提交上去，成片全程绿色像素占比 0.62~0.64、肤色像素 **0.00**，即通道不会
   凭空塞进人物；把提示词留空时套用的是设置里的 `default_prompt`，可以按需改成更强的
   「保持输入图主体、不要添加人物或图中没有的动作」。
3. **请求没走这条通道**。上游 `showcase` 通道（登录墙，见第 5 节）与第三方聚合服务返回的是
   素材库里的成片：素材库 22 条全部是人物类，其中三条的编排动作明确以「朝镜头/屏幕亲吻」
   （飞吻）收尾。若客户端里配了别的渠道或聚合服务，请核对实际请求的地址与模型名。

**Q：上游返回 401 `login_required`？**
说明用了 `showcase` 通道——该通道已被上游加上登录墙，匿名不可用。请把
`GATEWAY_ENDPOINT_MODE` 改回 `plain`。

**Q：任务失败提示 `Only JPG, PNG, or WEBP images are supported`？**
输入图片格式不被上游接受，或 multipart 分片的 `Content-Type` 不对。网关会自动嗅探
图片真实类型并设置正确的 `Content-Type`，请确认传入的确实是 JPG/PNG/WEBP。

**Q：改了 `.env` 但没生效？**
环境变量只在**首次启动**（数据文件不存在）时决定初始值。之后请在后台「运行设置」
里修改，或删除数据文件重新开始。

**Q：容器里的时间不对？**
设置 `TZ` 环境变量（compose 默认 `Asia/Shanghai`）。

**Q：想清空所有数据重来？**
`docker compose down -v`（会一并删除数据卷）。

---

## 与 Python 原版的差异

| 方面 | Python 原版 | 本 Go 版 |
| --- | --- | --- |
| 依赖 | FastAPI + uvicorn + httpx | **纯标准库**，无第三方依赖 |
| 存储 | SQLite（部分版本为内存） | 单文件 JSON，原子写入 + 去抖落盘 |
| 口令哈希 | 依赖第三方库 | 自实现 PBKDF2-HMAC-SHA256，21 万次迭代 |
| 后台管理 | 无 | 完整单页控制台（仪表盘/任务/密钥/设置/实测/账号） |
| API 密钥 | 静态配置 | 可增删改、可限速、可设有效期 |
| IPv6 XFF | 未支持 | **已实测并支持**，含对照键验证方法与后台开关 |
| 生成通道 | 走 `showcase` + prompt | 默认走 `plain`（`showcase` 已被上游登录墙拦截） |
| 提示词 | 始终发送写死的 `DEFAULT_PROMPT` | `prompt` 留空时套用设置里的 `default_prompt`（原先是死配置，从不读取），显式传入优先 |
| 内容级复读检测 | 无 | `video_sha256` / `duplicate_of`：**输入图不同却拿到相同字节**时标记并写 `WARN`，同图重渲染不误报 |
| 身份策略 | 同一地址产出两条视频（复用 2 次额度） | **一次生成一个身份**：上游把首帧图与任务绑在身份上，复用会让新请求拿到旧图或旧视频 |
| 幂等键 | 每次提交随机 UUID | 每次生成一个键（重试沿用，下一代必换） |
| 任务续跑 | 无 | 重启后自动接管未完成任务 |
| 视频缓存 | 直接透传 | 成功后落地缓存，上游回收后仍可下载 |
| 代理 | 支持 | 支持，且与 XFF 伪造互斥 |
| 容器网络 | — | 明确只用默认 bridge，不创建项目网络 |

行为上刻意保持一致的细节：时长别名映射（4-7→6s、8-12→10s、13-20→15s）、仅支持
9:16 竖屏、`X-Forwarded-For` 取第一个值、默认提示词与 User-Agent/Referer 伪装。

---

## 开发

```bash
go build ./...            # 构建
go vet ./...              # 静态检查
gofmt -l .                # 格式检查
go test ./...             # 全部单元测试（约 40 秒，不需要网络）
```

测试覆盖：口令与会话、环境变量与设置校验、随机地址生成与地址池、JSON 数据库与
保留策略、上游客户端（含错误分类、multipart 头、代理与伪造互斥）、任务编排
（并发闸门、重投、缓存、重启续跑）、XFF 实测方法（用可编程假上游验证「对照键」能
识破退回真实 IP 的情况）、以及全部 HTTP 接口与后台流程。

详细的上游接口分析见 [`docs/上游接口分析.md`](docs/上游接口分析.md)，
从拉取仓库开始的完整部署流程见 [`docs/部署指南.md`](docs/部署指南.md)，
部署与验证记录见 [`docs/部署与验证.md`](docs/部署与验证.md)，
版本变更见 [`CHANGELOG.md`](CHANGELOG.md)。
