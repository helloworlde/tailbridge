# TailBridge

TailBridge 是一个独立的 Go 命令行工具：它通过官方 `tsnet` 在进程内加入 Tailnet，把明确配置的 HTTP/TCP 服务映射到 macOS loopback。它不启动系统 VPN，不修改系统路由、DNS 或代理。

内置 Web UI 用于查看匿名运行状态、手动诊断上游连接，并生成、校验、复制简化路由 YAML。CLI 和 YAML 始终是唯一权威入口；Web UI 不写配置、不保存上游目标，也不触发 reload。

> TailBridge 不是 Tailscale 官方产品，也不绕过 Tailnet ACL、Grants、设备审批或密钥生命周期。

## 快速开始

安装 Go 1.26.6 或更高版本，在 TailBridge 项目根目录构建：

```sh
mkdir -p ./dist
go build -o ./dist/tailbridge .
./dist/tailbridge version
```

初始化并校验配置：

```sh
./dist/tailbridge init
./dist/tailbridge check
```

已有配置时保留 `dist/config.yaml`，直接运行 `check`，无需再次 `init`。配置和构建产物均不纳入 Git。

最小配置：

```yaml
version: 1

web_ui:
  listen: "127.0.0.1:18180"

profiles:
  - id: home
    hostname: tailbridge-macos

routes:
  - id: dashboard
    local_port: 18080
    upstream: "http://dashboard:3000"

  - id: ssh
    local_port: 10022
    upstream: "server:22"
```

单 profile 时可省略 `profile`。省略 `kind` 时，HTTP(S) URL 推断为 `http`，其余 `host:port` 推断为 `tcp`。`local_port` 始终规范化为 `127.0.0.1:<port>`。

启动并查看：

```sh
./dist/tailbridge start
./dist/tailbridge status
open http://127.0.0.1:18180/
```

## CLI

| 命令 | 作用 |
| --- | --- |
| `tailbridge init` | 在可执行文件同目录创建权限为 `0600` 的最小配置；拒绝覆盖 |
| `tailbridge check` | 严格解析、规范化并校验 YAML，不连接 Tailnet；已知校验边界见开发文档 |
| `tailbridge start` | 启动 tsnet profiles、本地 routes 和可选 Web UI |
| `tailbridge status [--json]` | 读取匿名运行状态 |
| `tailbridge doctor [-route ID] [--json]` | 通过运行中的进程诊断已配置的上游连接，需要启用 Web UI |
| `tailbridge version` | 显示 TailBridge 与官方 Tailscale 库版本 |

不传 `-config` 时，配置固定为可执行文件同目录的 `config.yaml`，不受当前工作目录影响。节点身份和运行状态默认保存在 `~/Library/Application Support/TailBridge/`。

在另一个终端运行 `./dist/tailbridge doctor`，或在页面路由行点击「检查」。HTTP 发送 HEAD、验证 TLS、不跟随重定向，显示响应码和耗时；TCP 只建立并关闭连接。每条检查最多 5 秒。HTTP 4xx 表示服务器可达，但不证明认证或业务可用；HTTP 5xx、连接失败和诊断接口错误会令 `doctor` 返回非零退出码。诊断不启动第二个 tsnet 节点，也不输出上游地址或响应正文。

## 能力与边界

- HTTP：反向代理、HTTPS 验证、私有 CA、SSE、WebSocket。
- TCP：SSH、数据库、VNC 等任意 TCP 客户端。
- SOCKS5：适合需要保留原始 Host、TLS SNI、Cookie Domain 或 WebAuthn origin 的应用。
- Subnet routes、MagicDNS、Direct、Peer Relay、DERP：使用官方 tsnet 数据面和控制面策略。
- Web UI：匿名状态、手动连接诊断和本地路由 YAML 助手，只允许 loopback。

不支持系统全局 VPN、自动代理/DNS/路由修改、Funnel、公网入口、热重载、Web UI 写配置或强制固定某个 Peer Relay。官方客户端没有稳定的 per-client Peer Relay 固定 API；TailBridge 只报告实际使用路径。

## 文档索引

- [文档导航](docs/index.md)
- [配置参考](docs/configuration.md)
- [Web UI 与隐私边界](docs/web-ui.md)
- [架构与开发](docs/development.md)
- [完整示例](example.yaml)

## 当前工程状态

TailBridge 是独立 Go module：`github.com/helloworlde/tailbridge`，固定依赖官方 `tailscale.com v1.102.5`。构建不需要 Tailscale 源码 checkout、系统 Tailscale 客户端或本地 `replace`。

目前面向 macOS，独立项目迁移已完成；生命周期、真实 Tailnet 验收及已知问题见[架构与开发](docs/development.md)。项目保留原有 BSD-3-Clause 许可及版权声明，见 [LICENSE](LICENSE)。
