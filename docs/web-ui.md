# Web UI 与隐私边界

启用：

```yaml
web_ui:
  listen: "127.0.0.1:18180"
```

启动后访问 `http://127.0.0.1:18180/`。省略 `web_ui` 即完全关闭；地址必须为 IPv4/IPv6 loopback，并且不能与 route listener 重复。

## 页面能力

- 查看进程、profiles、路由监听和连接计数。
- 查看 Direct、Peer Relay、DERP 的匿名活跃路径计数。
- 查看 MagicDNS、子网路由和健康问题数量。
- 手动检查已配置的 HTTP/TCP 上游，显示响应码、耗时、错误分类和检查时间。
- 填写路由名称、profile、类型、本地端口和目标。
- 使用 Go 后端的同一套规则校验，并复制简化 YAML。

## 刻意不支持

- 不读取或展示已配置的 upstream。
- 不展示 peer 名称、IP、DERP region、具体 subnet 或健康消息。
- 不写 `config.yaml`，不保存编辑器输入，不执行 reload/restart。
- 不提供远程访问、CORS 或公网管理模式。

浏览器只会把编辑器中的目标发送给本机校验接口，后端不记录请求体。复制 YAML 后手工写入配置，运行 `tailbridge check`，然后重启进程。

路由「检查」只发送路由 ID，后端使用当前进程的配置和现有 Tailnet 连接。HTTP 使用 HEAD，不跟随重定向；TCP 不发送业务数据。HTTP 4xx 仍表示服务可达，5xx 显示为异常；TCP 可达不等于业务认证成功。检查上限 5 秒，最多同时执行 4 条，不自动重试或后台轮询上游。结果不包含上游地址、响应正文、响应头或底层错误。刷新状态不会重新检查上游；页面重新加载或进程重启后清除检查结果。

CLI `tailbridge doctor [-route ID] [--json]` 通过同一 loopback 接口进行诊断，因此需要 TailBridge 运行且启用 Web UI。页面无法获取运行状态时将显示未知，并禁用检查按钮。

## Web 安全

- listener 仅允许 loopback，请求 Host 必须为 loopback/localhost。
- POST 校验同源；草稿校验请求体限制为 16 KiB，诊断为 1 KiB，均拒绝未知 JSON 字段和多段 JSON。
- 响应设置 CSP、`no-store`、`nosniff`、frame deny 和 `no-referrer`。
- UI 资源由 Go 二进制 `go:embed`，无 CDN、Node runtime 或第三方脚本。
