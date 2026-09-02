# wrapper-manager v2 跨平台方案：仿 wrapper-lite-qemu 的 Go QEMU 宿主启动器

> 目标：让 wrapper-manager 能在 Windows / macOS / Linux 上运行。方案是——整个
> wrapper-manager 跑在**单个 QEMU Linux guest** 内，宿主只需一个 Go 编写的
> QEMU 启动器（仿 [wrapper-lite-qemu](https://github.com/WorldObservationLog/wrapper/blob/lite/wrapper-lite-qemu.cpp)），
> guest 内跑 Linux 原生 wrapper-manager（继续用现有的 rootless/chroot 管理
> wrapper-lite 实例）。客户端只连 manager 一个端口，实例端口全在 guest 内部。

## 0. 架构总览

```
Windows / macOS / Linux 宿主
┌────────────────────────────────────────────────────────────┐
│  wrapper-manager-qemu (Go 宿主启动器)                        │
│   - 定位/下载 QEMU (qemu-system-x86_64)                      │
│   - 定位/下载 guest 镜像 (vmlinuz + initramfs + data.img)    │
│   - 启动 qemu 进程, hostfwd 宿主端口 -> guest manager 端口    │
│   - 持久化 data.img（manager 全部状态: 实例/token/账号）      │
│   - 信号处理: Ctrl-C / SIGTERM 优雅关闭 qemu                 │
└──────────────┬─────────────────────────────────────────────┘
               │ hostfwd (如 127.0.0.1:8080 -> guest 8080)
               ▼
┌────────────────────────────────────────────────────────────┐
│  QEMU guest (Linux x86_64)                                  │
│  vmlinuz-lite-qemu (复用上游内核)                            │
│  initramfs (自制轻量: busybox + 静态 wrapper-manager + init)  │
│                                                             │
│  guest 内:                                                  │
│    /data (data.img 挂载)                                    │
│      └── wrapper-manager -host 0.0.0.0 -port 8080           │
│            ├── 下载 wrapper-lite payload (rootless)          │
│            ├── 每账号一个 wrapper-lite-rootless 实例          │
│            └── 全部 HTTP API (/m3u8 /key /login /logout…)   │
└────────────────────────────────────────────────────────────┘
```

**关键设计**：guest 是完整 Linux 环境，wrapper-manager 不需要任何跨平台改造——
它看到的就是 Linux + rootless/chroot 可用。宿主启动器只负责 boot 和端口转发，
因此一次实现同时解决 Windows/macOS/Linux 的运行问题。

## 1. 与上游 wrapper-lite-qemu 的对照（仿什么、不仿什么）

| 上游 wrapper-lite-qemu | 本方案 (wrapper-manager-qemu) |
|---|---|
| C++ 跨平台 (CreateProcess/fork) | **Go** 跨平台 (os/exec) |
| boot 单个 lite guest | boot 单个 manager guest |
| hostfwd 宿主端口→guest lite 12340 | hostfwd 宿主端口→guest manager 8080 |
| data.img 持久化 lite 账号 | data.img 持久化 manager 全部状态 |
| qemu/bin 捆绑 firmware | 同（复用上游捆绑方式，Go 侧处理） |
| `--login` 一次性 guest | guest 内 manager 自带 HTTP login（两阶段 2FA） |
| 每实例一个 qemu（旧模型） | **单 qemu 承载完整 manager**（多账号在 guest 内） |

**不采用**：被废弃的 multi-instances/supervisor 变体（guest 内塞多实例管理端点）。

## 2. 组件与文件

### 2.1 宿主启动器 `cmd/wrapper-manager-qemu/main.go`（Go）

仿 wrapper-lite-qemu.cpp 的跨平台逻辑，用 Go 实现：

- **QEMU 定位**（顺序）：`-qemu-bin` flag / `QEMU_BIN` env → `PATH` → 捆绑
  `qemu/bin/`（可执行同目录）。
- **自动下载**：首次运行时若缺 QEMU 或 guest 镜像，从 nightly.link 下载
  （GitHub Actions artifact）。下载内容：
  - `wrapper-manager-qemu-linux-x86_64` 等平台包（含启动器 + qemu/bin 资产）
  - `vmlinuz-lite-qemu` + 自制 `wrapper-manager-initramfs.cpio.gz` + 初始 `data.img`
- **QEMU 参数**（对照上游 buildQemuArgs）：
  ```
  qemu-system-x86_64 -L <dir>/qemu/bin -accel <accel>
    -cpu max|host -m 512 -smp 2
    -kernel <dir>/vmlinuz-lite-qemu -initrd <dir>/wrapper-manager-initramfs.cpio.gz
    -append "console=ttyS0 quiet net.ifnames=0 biosdevname=0"
    -display none -serial stdio -no-reboot
    -nic user,model=e1000,hostfwd=tcp:127.0.0.1:<hostPort>-:<guestPort(8080)>
    -drive file=<persistent>/data.img,format=raw,if=virtio
  ```
  accel 自动选择：Linux KVM→TCG fallback；macOS HVF（Apple Silicon 需 TCG）→TCG；
  Windows WHPX→TCG fallback（同上游 autoAccel 逻辑）。
- **持久化目录**：`data.img` 保存在宿主用户目录/可执行目录旁的持久位置
  （如 `~/.wrapper-manager/data.img` 或 exe 旁 `data/`），升级不丢账号。
- **信号**：捕获 SIGINT/SIGTERM（Windows: ctrl-c handler）→ 优雅关 qemu
  （先 `poweroff` 信号或直接 terminate qemu 进程组）。
- **stdout 透传**：qemu `-serial stdio` 输出透传到宿主控制台（guest 日志可见）。

### 2.2 guest initramfs `guest/`（自制）

轻量 initramfs，结构仿上游但去掉 Android rootfs：

```
guest/
├── build.sh               # 构建 wrapper-manager-initramfs.cpio.gz
├── init                   # busybox sh init 脚本
└── overlay/
    ├── bin/busybox        # 静态 busybox（apt busybox-static 提供）
    └── wrapper-manager    # CGO_ENABLED=0 静态编译的 manager
```

`init` 脚本流程（仿上游 init）：
```sh
#!/bin/busybox sh
mount -t proc proc /proc
mount -t sysfs sysfs /sys
mount -t devtmpfs devtmpfs /dev
insmod /e1000.ko /virtio*.ko /ext4.ko 等   # 内核模块取自上游 initramfs
ip link set lo up; ip link set eth0 up
ip addr add 10.0.2.15/24 dev eth0
ip route add default via 10.0.2.2 dev eth0
mount -t ext4 /dev/vda /data               # data.img
exec /wrapper-manager -host 0.0.0.0 -port 8080
```

要点：
- manager 在 guest 内**运行时自下载** wrapper-lite payload（nightly.link，guest 有网），
  initramfs 保持轻量（~15MB 静态 manager + ~2MB busybox）。
- `-device-info`、`-proxy` 等如需可经 fw_cfg/cmdline 传入，或先固定默认。

### 2.3 镜像构建 `guest/build.sh`

1. 静态 busybox：`apt-get download busybox-static`（或系统 busybox 若静态）。
2. `CGO_ENABLED=0 go build -o overlay/wrapper-manager .`（在 v2 分支源码上构建）。
3. 用 `cpio` 打包 overlay + init 成 `wrapper-manager-initramfs.cpio.gz`。
4. 初始 `data.img`：`mke2fs` 创建空 ext4（64MB，同上游 mkdata.sh 思路）。

### 2.4 GitHub Actions workflow `.github/workflows/build-manager-qemu.yml`

仿上游 build-lite.yml，矩阵构建并上传 artifact（nightly.link 可取）：

| Job | 产物 |
|---|---|
| build-initramfs | `wrapper-manager-initramfs.cpio.gz` + 初始 `data.img`（复用上游 `qemu-assets` 的 vmlinuz？否——vmlinuz 直接随包分发给启动器下载） |
| build-launcher (linux/macos/windows x86_64) | 各平台 `wrapper-manager-qemu` + `qemu/bin`（QEMU + firmware，仿上游打包逻辑） |

- vmlinuz-lite-qemu：从上游 wrapper `qemu-assets` artifact 转发/引用（不重复构建内核）。

## 3. Git 分支策略（v2）

用户指定在 **v2 分支**实现。当前工作区 main 有未提交的 HTTP 重构（25 文件）。
建议：

1. 先把当前 HTTP 重构提交到 main（或直接在 main 基础上）——
   **需确认**：v2 分支基于哪个 commit？
   - 选项 A：把当前未提交重构先 commit 到 main，再从 main 拉 v2。
   - 选项 B：直接在当前工作区创建 v2 分支并提交重构 + QEMU 工作。
2. v2 分支内容：
   - 现有 HTTP 重构代码（manager 本体）
   - `cmd/wrapper-manager-qemu/`（宿主启动器，Go）
   - `guest/`（initramfs 构建）
   - `.github/workflows/build-manager-qemu.yml`
   - README 增补 QEMU 部署章节

## 4. 实施阶段

- **S1**：Go 宿主启动器骨架（QEMU 定位/参数/信号/端口转发），Linux 本机验证能
  boot 上游 lite guest（先用现有 qemu-assets 的 initramfs 验证 boot 链路）。
- **S2**：自制 manager initramfs（busybox + 静态 manager + init），本地构建并
  boot，验证 guest 内 manager 启动、/status 可达、wrapper-lite payload 下载、
  真实账号登录（复用 wm-real 测试方法）。
- **S3**：跨平台完善（Windows 信号/路径、macOS accel、QEMU 捆绑下载）。
- **S4**：GitHub Actions workflow + nightly.link 分发；README。
- **S5**：全平台冒烟（Linux 实测 + Windows/macOS 若环境可测则测）。

## 5. 验证清单

- Linux：`go build ./cmd/wrapper-manager-qemu` → 启动 → curl guest manager /status →
  `/login` 真实账号（含 2FA）→ `/m3u8` 真实歌曲 → `/logout` → Ctrl-C 优雅关停，
  data.img 保留账号，重启恢复。
- Windows/macOS：若本机不可测，靠 CI workflow 产物 + 文档说明。

## 6. 风险与备注

- vmlinuz 复用上游：若上游内核不含所需模块（virtio/ext4 均有），基本无风险。
- guest 内 manager 自下载 wrapper-lite payload 需要 guest 网络（已配 10.0.2.15 NAT）。
- data.img 单文件持久化：并发/掉电可能损坏，建议 guest init 里 ext4 容忍挂载
  （或定期快照）。升级 manager 二进制 = 换 initramfs，data.img 不动。
- 宿主启动器 Go 跨平台：syscall 差异集中在信号/进程组（Windows 用 job object 或
  直接 Kill），参考上游 C++ 的处理思路。
