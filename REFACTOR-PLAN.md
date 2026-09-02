# wrapper-manager 重构方案：基于 wrapper-lite HTTP API 的多账号管理网关

> 依据 https://github.com/WorldObservationLog/wrapper（默认分支 `lite`）重构本仓库
> （https://github.com/WorldObservationLog/wrapper-manager）。

## 0. 背景与目标

- 上游 wrapper 已整体切换为 **wrapper-lite**：单账号、单 HTTP 端口、内部自持解密上下文，
  提供 `GET /m3u8 /key /lyrics /webplayback /status` 与 `POST /license`，统一响应信封
  `{"code":0,"msg":"SUCCESS","data":{...}}`。
- wrapper-lite **没有**旧 wrapper 的 raw-sample TCP 解密端口；AppleMusicDecrypt v3 已改为
  直连 wrapper-lite（/key 取 ctx/state 模板 + 本地 Temari 解密）。
- 因此 wrapper-manager 的角色从“gRPC + 池化 sample 解密调度”变为
  **多账号 wrapper-lite 实例的启动/登录/登出管理 + wrapper-lite HTTP API 兼容的聚合网关**。

### 已确认的设计决策（用户拍板）

| 决策点 | 结论 |
|---|---|
| 解密接口 | **取消** decrypt 能力与 gRPC Decrypt；manager 不再做 sample 解密 |
| wrapper 获取 | GitHub Actions artifact 经 **nightly.link** 下载（linux-x86_64 / arm64 原生包） |
| 启动方案 | 沿用旧版“每账号一实例”，进程改为 `wrapper-lite-rootless`（非 qemu） |
| 运行权限 | 优先 rootless（user namespace），需在部署环境验证（WSL 实测 OK） |
| API 形态 | 完全转发式：manager 提供 lite 同款端点 + 新增 `/login` `/logout` |
| login/2FA | 两阶段轮询：`POST /login` 返回 `code=2` + pending，客户端再带 code 提交 |
| 数据布局 | 沿用 `data/wrapper/rootfs/data/instances/<id>/` 作为每账号 lite `--base-dir` |
| 就绪判定 | 服务模式启动后轮询该实例 HTTP `/status` 直至 regions 非空 |
| 响应格式 | 透传 lite 信封 `{code,msg,data}`；manager 错误自行拼 `{code:-1,msg}` |
| 交付范围 | 重构 wrapper-manager 本体 + 更新 README（含 curl 示例）；不动 AppleMusicDecrypt |

## 1. 目标架构

```
                       ┌──────────────────────────────────────────────┐
                       │               wrapper-manager (Go)           │
  HTTP 客户端          │                                              │
 (AppleMusicDecrypt    │  ┌────────────┐   ┌───────────────────────┐  │
  v3 / curl / 自定义)  │  │ HTTP Router│──▶│ Instance Registry     │  │
       │               │  │ /m3u8 /key │   │ id=UUIDv5(username)   │  │
       │  {code,msg,   │  │ /lyrics    │   │ region / port / proc  │  │
       ▼   data}       │  │ /webplaybk │   └──────────┬────────────┘  │
                       │  │ /license   │              │ 选中实例       │
                       │  │ /status    │   ┌──────────▼────────────┐  │
                       │  │ /login     │   │ 反向代理/转发          │  │
                       │  │ /logout    │   │ (透传信封, 按需改写    │  │
                       │  └────────────┘   │  adamId/region 路由)   │  │
                       └───────────────────┴────────────────────────┘
                                    │ fork/exec + 文件轮询
              ┌─────────────────────┼──────────────────────────┐
              ▼                     ▼                          ▼
   wrapper-lite-rootless      wrapper-lite-rootless     wrapper-lite-rootless
   --base-dir /data/instances/<id1>  (账号A)             (账号B) …
   --host 127.0.0.1 --port <随机>
        │ chroot+userns
        ▼
   rootfs/data/instances/<id>/{STOREFRONT_ID,MUSIC_TOKEN,DEV_TOKEN,
                               token_cache.json,2fa.txt,…}
```

- 每个已登录账号 = 一个常驻 `wrapper-lite-rootless` 子进程（服务模式），监听本机随机端口。
- manager 对外是一个 HTTP 服务；按 `adamId` 的区域可用性/账号 region 挑选实例后把请求
  转发到该实例端口，并把实例响应（信封）透传回客户端。
- manager 同时负责账号登录（一次性 `--login` 子进程）、登出、崩溃自动重启、启动恢复。

## 2. wrapper-lite 获取与部署（nightly.link）

- 来源：`https://nightly.link/WorldObservationLog/wrapper/workflows/build-lite/lite/wrapper-lite-linux-x86_64.zip`
  （arm64 对应 `wrapper-lite-linux-aarch64`，若上游 workflow 产出则同规则）。
- 首次启动（或 `data/wrapper` 缺 `wrapper-lite-rootless`）时下载 → 解压到 `data/wrapper/`
  （zip 顶层含 `wrapper-lite`、`wrapper-lite-rootless`、`rootfs/`）。
- `-mirror` 开关保留：下载失败/限速时可提示手动放置或走代理（沿用现有 GetHttpClient 代理逻辑）。
- `-prepare` 保留：仅完成下载解压后退出（供打包/预置镜像）。

## 3. 实例生命周期（启动方案）

沿用旧版状态机与数据结构骨架，替换 wrapper 进程与驱动方式：

1. **实例 ID**：`uuid.NewV5(namespace, username)`，与旧版一致；目录名 = ID。
2. **登录（WrapperInitial → LiteLogin）**：
   - 建 `data/wrapper/rootfs/data/instances/<id>/`；
   - `exec wrapper-lite-rootless --login user:pass --code-from-file --base-dir /data/instances/<id>`
     （工作目录 `data/wrapper/`），登录子进程一次性退出；
   - 2FA：lite 检测到需要 2FA 时轮询 `base-dir/2fa.txt`（最多 60s）。manager 发现登录进程
     未在短时限内退出且目录无 token 文件 → 判定需要 2FA，向 HTTP login 客户端返回
     `code=2, msg="2fa code require", data:{loginId}`；客户端再 `POST /login` 带 code，
     manager 写 `rootfs/data/instances/<id>/2fa.txt`；
   - 子进程退出码 0 且 token 文件（STOREFRONT_ID / MUSIC_TOKEN）存在 → 登录成功；
     否则失败 → 清理数据目录，向客户端返回 `code=-1`。
   - login 状态机可同时用：进程退出码为主 + base-dir 文件（token/2fa.txt）轮询为辅。
3. **就绪（wrapperReady）**：
   - 启动服务模式：`wrapper-lite-rootless --base-dir /data/instances/<id> --host 127.0.0.1
     --port <GenerateUniquePort>`；
   - 轮询 `GET http://127.0.0.1:<port>/status` 直至返回 regions 非空或超时；
   - region 直接取 `/status` data.regions[0]（lite 已归一化，不再需要
     storefront_ids.json 下载/解析；旧 `parseStorefrontID` 删除或仅留兜底）；
   - 注册进 `Instances` 表并写 `data/instances.json`。
4. **进程监控（wrapperDown 沿用）**：服务进程退出 → 从表移除；`NoRestart=false` 时自动重启
   （重新启动服务模式并等 /status），`NoRestart=true`（登出）时清理并落盘。
5. **启动恢复**：读 `data/instances.json`，逐个拉起服务模式（不重新登录）。

## 4. HTTP API 设计

统一信封：成功 `{"code":0,"msg":"SUCCESS","data":{...}}`；失败 `{"code":-1,"msg":"<原因>","data":null}`。
content-type 一律 `application/json`。

### 4.1 资源端点（与 wrapper-lite 兼容，多账号聚合）

| Method | Path | 参数 | 行为 |
|---|---|---|---|
| GET | `/m3u8` | `adamId` | 按 adamId 区域可用性选实例 → 转发 `GET /m3u8?adamId=` → 透传 |
| GET | `/key` | `adamId`, `uri?` | 选实例 → 转发 `GET /key` → 透传（含 ctx/state/寄存器） |
| GET | `/lyrics` | `adamId`, `language?`, `syllable?` | 选实例（优先含歌词）→ 转发 → 透传 |
| GET | `/webplayback` | `adamId` | 选实例 → 转发 → 透传 |
| POST | `/license` | JSON `{adamId,challenge,uri}` | 选实例 → 转发 → 透传 |
| GET | `/status` | — | 聚合所有已登录实例 regions 与账号数，`data:{status,regions,clientCount,ready}` |

实例选择复用旧 `SelectInstance` 思路：对每个候选实例的 region 用 `checkAvailableOnRegion`
（Apple catalog 探测，带 SongRegionCache/singleflight）过滤；没有可用实例返回
`code:-1,msg:"no available instance"`（客户端可重试）。`/lyrics` 额外用旧
`SelectInstanceForLyrics`（HEAD 探测）优先选有歌词的实例。

### 4.2 管理端点（新增）

| Method | Path | Body | 行为 |
|---|---|---|---|
| POST | `/login` | JSON `{username,password,code?}` | 无 code：启动登录 → 立即返回 `code:0`(成功) 或 `code:2`(需 2FA, `data:{loginId}`) ；带 code：写入 2fa.txt 完成登录，轮询至终态返回成功/失败 |
| POST | `/logout` | JSON `{username}` | 置 NoRestart → kill 服务进程 → 删除 `rootfs/data/instances/<id>` → 从 instances.json 移除 → `code:0` |

- `/login` 幂等：同 username 已有实例返回 `code:-1,msg:"already login"`。
- 2FA 采用两阶段；manager 侧给每个进行中的登录一个 `loginId`（= instance id），
  客户端用同一 username+password+code 再次 POST 完成。
- `/logout` 对不存在账号返回 `code:-1,msg:"no such account"`。

### 4.3 移除

- gRPC 服务、`proto/` 生成代码、`Decrypt`/`DecryptInstance`/`Dispatcher`（解密已取消）、
  `structs.go` 中 bson 旧协议类型、`m3u8.go`（直连旧 wrapper TCP）、`webplay.go`/
  `lyrics.go` 中的直连 Apple 调用大多不再需要（改为转发 lite；`checkAvailableOnRegion`
  与歌词 HEAD 探测所需的 dev token 逻辑 `token.go GetToken` 视需要保留，因为 region
  探测仍需 Apple catalog Bearer token；`GetMusicToken` 改为读 base-dir 下文件——若保留
  直连探测路径才需要）。
- `creack/pty` 依赖移除（lite 不需要 pty 交互）；root 检查放宽为“root 或可 user ns”。

## 5. 文件级重构映射

| 现有文件 | 处置 |
|---|---|
| `main.go` | 重写：flag 同旧（`-host -port -mirror -debug -proxy -prepare -device-info`）+ HTTP mux 注册全部路由；去掉 grpc/root panic |
| `wrapper.go` | 改 `PrepareWrapper`（nightly.link 下载解压）、`LiteLogin`、`LiteStart`（服务模式）、`LiteReady`（/status 轮询）、沿用 `handleOutput`→改为进程 stdout 简单记录 + 文件轮询；`DownloadWrapperRelease` 改 nightly.link |
| `instance.go` | 沿用 `WrapperInstance{Id,Region,Port,Cmd,NoRestart}`（去掉 DecryptPort/M3U8Port，改单一 `Port`）+ 序列化持久化 |
| `handler.go` | 重写为 HTTP 响应助手 + 登录状态机（LoginConnMap → `LoginStateMap`) |
| `decrypt*.go` | 删除 |
| `structs.go` | 删除（旧 bson 协议）或清理为 HTTP 请求/响应结构 |
| `m3u8.go` | 删除（旧 TCP 直连）；转发逻辑并入 proxy 模块 |
| `token.go` | 保留 `GetToken`（region 探测用）+ 读 base-dir 文件的新函数 |
| `region.go` | 保留 `checkAvailableOnRegion` / `SelectInstance` / `SelectInstanceForLyrics`（现在作用于 HTTP 实例的 region 字段） |
| `lyrics.go` / `webplay.go` | 直连 Apple 的 Lyrics/License/WebPlayback 移除（由 lite 承担）；只留歌词 HEAD 探测（可留可删，取决于是否用旧 SelectInstanceForLyrics 路径） |
| `port.go` | 沿用 `GenerateUniquePort`（单端口即可） |
| `proto/` | 删除 |
| `Dockerfile` / `docker-compose.yml` | 更新：镜像内需要 `unshare`/userns 支持（`--privileged` 或 seccomp 放开），数据卷改为 `data/`；移除 EXPOSE gRPC 8080 → HTTP 端口 |
| `README.md` | 更新：新 HTTP API、nightly.link 部署、login/logout curl 示例 |
| `go.mod` | 移除 grpc/protobuf/pty/go-unzip(gofrs uuid 保留) 等不再用的依赖，加 `go mod tidy` |

## 6. 分阶段实施（可派子代理并行）

- **P1 骨架与实例管理**：wrapper.go 下载解压（nightly.link）、LiteLogin/LiteStart/LiteReady、
  instance 表/持久化/自动重启、port.go。
- **P2 HTTP API**：main.go mux + `/status /m3u8 /key /lyrics /webplayback /license` 转发 +
  实例选择（region.go），透传信封。
- **P3 管理端点**：`/login`（两阶段 2FA 状态机）、`/logout`、登录并发锁。
- **P4 清理与构建**：删 gRPC/decrypt/proto/pty 等，go.mod tidy，Dockerfile/compose 更新，
  README 重写，`go vet`/`gofmt`/`go build` 通过。

## 7. 验证

- `go build .`、`go vet ./...` 通过；无测试套件（沿用现状）。
- 环境实测（本 WSL 已证）：
  - 非 root 启动 `wrapper-lite-rootless` 服务模式 OK，`/status` 返回信封；
  - nightly.link zip 可下载解压。
- 手工冒烟：`-prepare` 后启动 manager → `curl /status`（regions 空）→ 登录一个测试账号
  （或跳过真实 Apple 登录，仅验证到 2FA pending/失败路径）→ `/logout` 清理。
- 真实 Apple 登录需用户提供账号凭据/2FA，由用户决定是否进行端到端验证。

## 8. 风险与备注

- wrapper-lite 目前无正式 release，依赖 nightly.link 的 artifact 保留策略（最近一次成功
  workflow 运行），若上游长时间不跑 workflow 需手动放包。
- 2FA 流程依赖 lite 对 `2fa.txt` 的 60s 轮询窗口；manager 侧需要把“等待 2FA”与“登录失败”
  区分开（进程未退出 + 无 token + lite 日志出现 2fa 提示 3 种信号综合判断）。
- region 探测仍直连 Apple amp-api，需要公网可达与 dev token（保留 GetToken）。
- 若部署环境 user namespace 被禁（部分容器），需 root 或 `--privileged` 运行 manager。
