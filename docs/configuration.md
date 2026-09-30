# 配置参考

配置采用严格 YAML schema。未知字段、重复 ID、监听冲突、非法 URL 和非 loopback 默认监听都会在 `tailbridge check` 阶段失败。

## 最小配置

```yaml
version: 1
web_ui: {listen: "127.0.0.1:18180"}
profiles: [{id: home, hostname: tailbridge-macos}]
routes:
  - {id: web, local_port: 18080, upstream: "http://service:3000"}
```

## 顶层字段

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `version` | 必填 | 当前只接受 `1` |
| `startup_timeout` | `2m` | profile 登录/启动超时 |
| `drain_timeout` | `15s` | 停止时等待连接退出的时间 |
| `runtime_dir` | Application Support | 状态文件目录 |
| `web_ui.listen` | 关闭 | 内置 UI 的 loopback 地址 |
| `profiles` | 必填 | 一个或多个独立 tsnet 节点 |
| `routes` | 空 | 本地 HTTP/TCP 映射 |

## Profile

```yaml
profiles:
  - id: home
    hostname: tailbridge-macos
    ephemeral: false
    socks5: false
    udp_port: 0
    advertise_tags: ["tag:tailbridge"]
    derp_check_region: "sin"
```

- `state_dir`：省略时按 profile ID 放在 Application Support；同一目录不能被两个进程同时使用。
- `control_url`：省略时使用 Tailscale 默认控制面。
- `socks5`：启动后把一次性 loopback 地址和凭据 JSON 写到 stdout，状态文件不保存密码。
- `udp_port`：`0` 为自动选择；固定值只适合有明确防火墙/NAT 要求的场景。
- `advertise_tags`：请求控制面授予标签，最终权限仍由 Tailnet policy 决定。
- `derp_check_region`：启动时及每 5 分钟诊断指定 DERP region ID/code；不改变选路。

不要在 YAML 中保存 Auth Key。首次自动注册可临时使用 `TS_AUTHKEY` 环境变量，成功后立即清除。

## 路由简写

```yaml
routes:
  - id: ssh
    local_port: 10022
    upstream: "server:22"
```

- 只有一个 profile 时，省略 `profile` 自动使用它；多个 profile 时必须指定。
- HTTP(S) URL 自动推断为 `http`，其他 `host:port` 自动推断为 `tcp`。
- `local_port: 10022` 等价于 `listen: "127.0.0.1:10022"`。
- `listen` 与 `local_port` 不能同时出现。

## HTTP 高级字段

```yaml
routes:
  - id: secure-web
    profile: home
    kind: http
    listen: "127.0.0.1:18443"
    upstream: "https://service:443"
    host_header: upstream
    tls_server_name: service.example
    ca_file: ./private-ca.pem
    connect_timeout: 10s
    request_timeout: 0s
    idle_timeout: 0s
```

HTTPS 始终验证证书；`ca_file` 只追加 CA。WebSocket/SSE/长请求应保持 `request_timeout: 0s`。`host_header` 可选 `upstream` 或 `preserve`。

## TCP 高级字段

```yaml
routes:
  - id: vnc
    profile: home
    kind: tcp
    listen: "127.0.0.1:15900"
    upstream: "desktop:5900"
    connect_timeout: 10s
    idle_timeout: 0s
```

TCP route 不接受 HTTP/TLS 专用字段。

## 非 loopback 监听

Route 只有显式设置 `allow_non_loopback: true` 才能监听局域网或所有接口。这会把 Tailnet 服务暴露给本机外部网络，通常不应启用。Web UI 永远不允许非 loopback 监听。
