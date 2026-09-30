# TailBridge 文档

TailBridge 的产品形态是 **Go CLI + 可选的内置只读 Web UI**。配置由 YAML 维护，CLI 负责校验和运行，Web UI 负责观察与辅助生成路由。

## 使用者

1. [配置参考](configuration.md)：最小配置、路由简写、高级字段、Relay 行为。
2. [Web UI 与隐私边界](web-ui.md)：页面能力、访问方式、安全限制。
3. [README 快速开始](../README.md)：构建、初始化、启动和常用命令。

## 开发者

1. [架构与开发](development.md)：模块职责、数据流、测试和发布前事项。
2. [示例配置](../example.yaml)：包含常用 profile 和 route 字段。

## 单一事实来源

- `config.yaml`：用户配置。
- `Runtime.Snapshot()`：运行状态。
- `status.json`：Snapshot 的本地持久化 adapter，供 `tailbridge status` 使用。
- Web UI：Snapshot 的只读 HTTP adapter，不是第二套控制面。
