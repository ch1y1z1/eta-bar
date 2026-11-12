# 原生前端

`eta.bar` 是原生 Metrix.Bar dashboard，ID 为 `eta-sequential-prototype`，版本为 `0.2.2`。从应用导入本地文件即可。默认 `demo` 为纯合成数据，不访问 API，不依赖网页或 npm。设置支持路径直读，保留分段圆环、单个刷新按钮与 30 秒调度。

## 输入

| 输入 | 含义 |
| --- | --- |
| `mode` | `demo`（默认）或 `api` |
| `baseURL` | Go API 纯 origin，例如 `http://localhost:8080`；不能包含路径、查询、片段或用户凭据 |
| `sourceHost` | `local`（默认）或后端预设 SSH profile key，不能填写任意主机名 |
| `logPath` | 后端容器或预设 SSH 主机上的绝对日志路径；默认空，填写后自动识别 run_id |
| `runID` | 仅 logPath 留空时使用，默认 `demo-run` |
| `token` | secret 类型输入；请求时作为 `Authorization: Bearer …`，对应后端环境变量 `ETA_API_TOKEN` |
| `staleSeconds` | 源快照过期阈值，秒，默认 120 |
| `etaValiditySeconds` | 阶段 ETA 独立有效期，秒，默认 120 |

`token` 只通过 Metrix.Bar 的 secret 输入设置，不是普通文本输入，也不是 URL 参数。面板不会显示上游错误正文或凭据。远程部署优先 HTTPS；源码为输入 origin 声明 `allow-http=true`，支持 WSL 的非 loopback HTTP。Mac 的 baseURL 应填写直接可达的后端 origin；公开默认值为 `http://localhost:8080`，无部署私有地址。Metrix.Bar 0.1.2 / protocol 0 使用 `$metrix.secret('token')` 读取 secret，Text 使用 `textColor`。

## 快照接口契约

logPath 非空时请求 `GET {baseURL}/v1/snapshot?host={encodeURIComponent(sourceHost)}&path={encodeURIComponent(logPath)}`；runID 不参与路径请求或返回数据匹配。后端必须设置 token，local 路径必须位于 `local_allowed_roots` 内，SSH 路径必须位于该 profile 的 `allowed_root` 内。容器示例填写 `sourceHost=local`、`logPath=/logs/monitor.jsonl`。logPath 留空时请求 `GET {baseURL}/v1/runs/{encodeURIComponent(runID)}/snapshot`。两种请求均超时 8 秒、拒绝重定向。mode、origin、runID、host、path 组成源标识；任一变化会清除旧源数据，旧请求返回不会覆盖新源。响应 envelope 示例：

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

`data` 在未得到首条快照时可为 `null`；已有数据时为符合 `schema/snapshot.schema.json` 的完整快照对象。`collection.state` 为 `ok` 或 `unreachable`（窗口内没有完整记录时 error.code 为 `not_ready`）；`collection.window` 为 `bounded_tail`。`last_success_at` 可为 `null`，错误为 `null` 或安全的 code/message 对象，例如 `{ "code": "source_unavailable", "message": "Source is unavailable" }`。采集失败可返回最后有效快照并标记 `unreachable`。例中固定时间仅解释格式，不表示当前健康状态。

源快照的 `timestamp` 决定数据新鲜度，不能被服务端响应时间替代。前端对同 attempt 的序号倒退、同 seq 内容冲突、进度倒退、任务列表变化及终态回退进行拒绝；切换 attempt 时接受新的尝试。最多显示 200 个任务。

## 设计与限制

整体 67/150 舍入为 45%，ETA 将两个未完成阶段的预算 480+900 秒相加得到 23 分钟。阶段圆弧按 total 分配而非按 ETA 分配；同 unit 且所有 total 已知才汇总。阶段间间隔用于视觉区分，不改变数值进度。界面使用主题色及原生组件，保留单个刷新按钮和 30 秒调度。

断连、源数据过期或明显未来时间会冻结估计并保留最后值；ETA 自身过期显示未知。冻结值是上次预算，不是当前有效预测。没有本地倒计时，也不从工作量比例反推耗时。默认 demo 的心跳会重申同一固定合成预算，不模拟真实实验推进。

服务端接口及本地启动步骤见[根目录 README](../README.md)。Metrix.Bar 内的导入与显示验证需在应用中执行；HTTP 接口验证不替代应用兼容性验证。公开参考：[格式](https://metrix.bar/docs/bar-format.md)、[组件](https://metrix.bar/docs/components.md)。
