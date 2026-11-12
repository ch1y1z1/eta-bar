# 原生前端

`eta.bar` 是原生 Metrix.Bar dashboard，ID 为 `eta-sequential-prototype`，版本为 `0.2.1`。从应用导入本地文件即可。默认 `demo` 为纯合成数据，不访问 API，不依赖网页或 npm。源码保留现有绘图与功能，仅调整设置和文档中的说明文字。

## 输入

| 输入 | 含义 |
| --- | --- |
| `mode` | `demo`（默认）或 `api` |
| `baseURL` | Go API 纯 origin，例如 `http://localhost:8080`；不能包含路径、查询、片段或用户凭据 |
| `runID` | 对应后端 run ID，默认 `demo-run` |
| `token` | secret 类型输入；请求时作为 `Authorization: Bearer …`，对应后端环境变量 `ETA_API_TOKEN` |
| `staleSeconds` | 源快照过期阈值，秒，默认 120 |
| `etaValiditySeconds` | 阶段 ETA 独立有效期，秒，默认 120 |

`token` 只通过 Metrix.Bar 的 secret 输入设置，不是普通文本输入，也不是 URL 参数。面板不会显示上游错误正文或凭据。远程部署优先 HTTPS；保留源码的网络权限声明，不为了 HTTP 接入扩大权限。

## 快照接口契约

前端请求 `GET {baseURL}/v1/runs/{encodeURIComponent(runID)}/snapshot`，超时 8 秒，拒绝重定向。响应 envelope 示例：

```json
{
  "collection": {
    "state": "ok",
    "observed_at": "2026-01-01T00:00:00.000Z",
    "last_success_at": "2026-01-01T00:00:00.000Z",
    "error": null,
    "window": "bounded_tail"
  },
  "data": null
}
```

`data` 在未得到首条快照时可为 `null`；已有数据时为符合 `schema/snapshot.schema.json` 的完整快照对象。`collection.state` 为 `ok`、`unreachable` 或 `not_ready`（窗口内没有完整记录）；`collection.window` 为 `bounded_tail`。`last_success_at` 可为 `null`，错误为 `null` 或安全的 code/message 对象，例如 `{ "code": "source_unavailable", "message": "Source is unavailable" }`。采集失败可返回最后有效快照并标记 `unreachable`。例中固定时间仅解释格式，不表示当前健康状态。

源快照的 `timestamp` 决定数据新鲜度，不能被服务端响应时间替代。前端对同 attempt 的序号倒退、同 seq 内容冲突、进度倒退、任务列表变化及终态回退进行拒绝；切换 attempt 时接受新的尝试。最多显示 200 个任务。

## 设计与限制

整体 67/150 舍入为 45%，ETA 将两个未完成阶段的预算 480+900 秒相加得到 23 分钟。阶段圆弧按 total 分配而非按 ETA 分配；同 unit 且所有 total 已知才汇总。阶段间间隔用于视觉区分，不改变数值进度。界面使用主题色及原生组件，保留单个刷新按钮和 30 秒调度。

断连、源数据过期或明显未来时间会冻结估计并保留最后值；ETA 自身过期显示未知。冻结值是上次预算，不是当前有效预测。没有本地倒计时，也不从工作量比例反推耗时。默认 demo 的心跳会重申同一固定合成预算，不模拟真实实验推进。

服务端接口及本地启动步骤见[根目录 README](../README.md)。Metrix.Bar 内的导入与显示验证需在应用中执行；HTTP 接口验证不替代应用兼容性验证。公开参考：[格式](https://metrix.bar/docs/bar-format.md)、[组件](https://metrix.bar/docs/components.md)。
