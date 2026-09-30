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
- 填写路由名称、profile、类型、本地端口和目标。
- 使用 Go 后端的同一套规则校验，并复制简化 YAML。

## 刻意不支持

- 不读取或展示已配置的 upstream。
- 不展示 peer 名称、IP、DERP region、具体 subnet 或健康消息。
- 不写 `config.yaml`，不保存编辑器输入，不执行 reload/restart。
- 不提供远程访问、CORS 或公网管理模式。

浏览器只会把编辑器中的目标发送给本机校验接口，后端不记录请求体。复制 YAML 后手工写入配置，运行 `tailbridge check`，然后重启进程。

## Web 安全

- listener 仅允许 loopback，请求 Host 必须为 loopback/localhost。
- POST 校验同源；请求体限制为 16 KiB，拒绝未知 JSON 字段。
- 响应设置 CSP、`no-store`、`nosniff`、frame deny 和 `no-referrer`。
- UI 资源由 Go 二进制 `go:embed`，无 CDN、Node runtime 或第三方脚本。
