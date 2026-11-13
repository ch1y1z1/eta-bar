# 原生前端

`eta.bar` 是原生 Metrix.Bar dashboard，ID 为 `eta-sequential-prototype`，版本为 `0.3.0`。从应用导入本地文件即可。默认 `demo` 为纯合成数据，不访问 API，不依赖网页或 npm。设置支持路径直读，保留分段圆环、单个刷新按钮与 30 秒调度。

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

## 本机 ETA 历史（0.3.0）

分段圆环和阶段列表下方增加原生短折线图，不增加按钮。仍通过原有接口每 30 秒采集，手动刷新也可触发观测。只记录通过校验、源时间新鲜、采集状态正常且所有未完成阶段 ETA 有效的整场剩余预算。不会减少预算来模拟倒计时。

每条点的横坐标是 Mac 实际接受观测的 Unix 毫秒时间，`Chart.xKind=time` 保留不规则间隔的真实比例；纵坐标为分钟，从零开始。原生轴提供紧凑时间与数值刻度，悬停显示分钟。相同未完成阶段的 ID、ETA `updated_at` 和 `remaining_seconds` 组合仅保存一次；仅 seq 或快照心跳改变不会增加点。合成 demo 每次心跳明确重申 23 分钟预算，所以曲线保持水平。

未知 ETA、ETA 过期、源时间过期或异常、无效响应、采集失败会设置断点；实际观测相隔超过 120 秒也会断线。恢复时只有新的有效 ETA 才追加点，并以独立 Chart series 绘制，绝不跨中断连线。单点用圆点显示。确认整场 succeeded 且观测有效时记录一次零；失败或取消不产生零。render 不追加有效观测，也不会以冻结值补历史。

历史使用 `<script apis="localStorage">` 的同步 `localStorage.getItem/setItem`，key 为 `eta-history-v1`，由 Metrix.Bar 保存在当前 dashboard 的本机存储中，Worker 重建后恢复。记录按输入源（mode、origin、runID、host、path）及实际 run_id / attempt_id 隔离；切换源清空当前快照，必须先收到该源有效快照才选择相应历史，不将磁盘历史当作实时进度。请求携带 Worker 内源代数，A→B→A 时早期 A 请求也不能写入新代数的历史。

保留最近 24 小时，每次尝试至多 720 点，每源至多 8 个 run/attempt，至多 8 个源；另将序列化 JSON 限制为 1,000,000 个字符（UTF-8 最坏约 4 MB）。先淘汰最久未观测的源/尝试，必要时缩短最后一条历史。读取时清理无效、重复、越界和过期记录；损坏 JSON 安全降级为空会话。读取或写入失败不影响实时进度，图下注明本地历史不可读或未保存，不声称已持久化。后续成功写入会恢复持久化状态。

只保存源标识、run/attempt 标识、ETA 指纹、观测值及断点，不保存 token、Authorization、上游正文、完整快照或工作量。源路径等身份信息仅进入用户 Mac 的本地存储，不随 `.bar` 源码分发。导出源码不会携带历史；清除 dashboard 数据或更换 dashboard 实例可能丢失历史。存储容量不足时只保证当前会话可用。

这是采样历史：没有服务端 history API，不会回填应用退出、Mac 休眠或未观测期间的记录；保留上限也意味着 24 小时内不保证每条观测都保留。断线以多条独立 series 表示，未插值缺失值。Native Chart 使用 `series[].points[{x,y,valueLabel}]`、`xKind`、`xDomain/yDomain`、`xTicks/yTicks`、`dots`、`curve=linear`；安装环境须通过 `/v0/types`、`/v0/validate` 和 `/v0/check` 核对这些属性及 localStorage opt-in。
