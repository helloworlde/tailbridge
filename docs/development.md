# 架构与开发

TailBridge 是 Go CLI，不是原生 macOS App。Web UI 是可选的本地观察 adapter，与 CLI 共用相同配置和运行状态。

源码使用独立 module `github.com/helloworlde/tailbridge`，通过 `go.mod` 固定依赖官方 `tailscale.com v1.102.5`，无需 Tailscale checkout。module 路径是项目标识，不表示远程仓库已经创建或发布。

## 模块

| 模块 | 职责 |
| --- | --- |
| `main.go` / `doctor.go` | CLI 命令分发、配置路径、进程信号和运行中路由诊断 |
| `internal/config` | 严格 YAML、默认值、简写规范化、跨字段校验 |
| `internal/app` | profile/route/Web UI 生命周期和唯一 Snapshot |
| `internal/forward` | HTTP/TCP 两个转发 adapter，共用 Dialer seam |
| `internal/diagnostics` | 保守错误分类和凭据脱敏 |
| `internal/webui` | 内嵌资源、匿名状态 API、手动诊断和路由草稿校验 |

`Runtime.Snapshot()` 是状态接口。`status.json`、CLI 和 Web UI 都消费该接口；不得各自重新拼装网络状态。

## 数据边界

状态接口禁止加入 profile hostname、Tailnet IP、peer 名称/IP、upstream、DERP region、subnet 明细、健康原文、底层错误、Auth Key 或 SOCKS 密码。新增状态字段时必须同步更新隐私测试和 Web UI API 测试。

## 验证

```sh
go test ./...
go vet ./...
go test -race ./...
go build -o dist/tailbridge .
./dist/tailbridge check -config example.yaml
```

浏览器验收至少覆盖 desktop/窄屏、状态轮询、HTTP/TCP 草稿、合法/重复/非法校验、安全响应头、非法 Host、跨 Origin POST 和敏感字段检查。

CI 使用 macOS、`go.mod` 指定的 Go 版本，执行依赖下载与完整性检查、vet、竞态测试、构建和示例配置校验。该流程仅验证离线功能，不创建 Tailnet 节点。

## 从原目录迁移

默认配置是可执行文件同目录的 `config.yaml`。将已有配置放在 `dist/config.yaml` 并保持 `0600` 权限；它由 `.gitignore` 排除。默认节点身份和状态继续使用 `~/Library/Application Support/TailBridge/`，不需要重新注册节点。

如果配置显式指定相对 `state_dir`、`runtime_dir` 或 `ca_file`，这些路径以配置文件目录为基准，迁移配置时需要检查其解析结果。同一 profile 的身份目录不能被两个进程同时使用。

## 生命周期与发布前验证

运行目录和每个 profile 的身份目录均使用进程锁。重复启动不会覆盖现有实例的状态；启动失败会释放已准备的监听器和已启动的 profiles。TCP 在关闭超时后取消未完成的拨号；HTTP 在排空超时后关闭残留请求。最终状态在后台状态写入器停止后保存。

自动化测试覆盖真实本地监听、HTTP/TCP 转发、SSE/协议升级、TLS 信任链与 SNI、诊断 API、启动失败回收、重复启动和最终状态。生命周期测试只替换外部 Tailscale SDK，因而不能证明真实登录、设备审批、重连、SOCKS5、MagicDNS、子网访问和多真实 Tailnet 的行为；这些仍需相应环境验收。

`doctor` 与 Web UI 共用同一个诊断实现、HTTP transport 配置和现有 profile Dialer。请求仅能指定运行时已经配置的路由 ID；并发检查上限 4，每次上限 5 秒。检查结果只在调用方返回，页面保留本次会话结果与时间，不进行自动上游探测。它检查上游可达性，不替代通过本地映射端口的业务验收。

升级 Tailscale 时同时验证登录状态解析与 DERP 诊断：前者目前依赖日志文本，后者使用的 `DebugDERPRegion` 在 SDK 中标为不稳定 API。业务转发继续通过现有 `Dialer` 接口隔离。

## 暂不实现

- Web UI 配置写入、热重载和远程管理；
- 系统 VPN、代理、DNS 或路由修改；
- Funnel、Peer Relay 服务端；
- SwiftUI 菜单栏、登录项、签名和公证。
