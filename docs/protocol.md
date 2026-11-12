# 快照协议与设计规范 1.0

本协议描述通用串行任务的可观测状态，不包含实验业务或身份信息。机器字段以 `schema/snapshot.schema.json` 为准；Schema 之外的跨字段、生命周期约束由采集端验证。示例全部合成。

## JSONL 完整快照

日志使用 UTF-8，每条记录是一个完整 JSON 对象，以换行结束。每次心跳重复完整状态，不发送增量；消费者以整条替换，不能将新旧字段混合。Python 记录器限制单条 JSON 为 256 KiB；采集窗口同为 256 KiB，还需容纳换行及完整记录，接入时应让单行明显小于窗口，避免大行无法完整落入窗口。`examples/demo/monitor.jsonl` 是完整单行样例。

| 字段 | 含义与单位 |
| --- | --- |
| `schema_version` | 固定 `1.0` |
| `run_id` | 逻辑任务组标识，不包含账号、路径或身份 |
| `attempt_id` | 一次执行标识；重跑必须换值 |
| `seq` | 同 attempt 从 1 起严格递增的整数 |
| `timestamp` | 源端本次快照/心跳时间，UTC，结尾 `Z` |
| `started_at` | 本 attempt 开始时间，UTC，整个 attempt 不变 |
| `last_progress_at` | 可选，实际工作量上次变化时间，UTC |
| `state` | `pending`、`running`、`succeeded`、`failed`、`cancelled` |
| `execution` | 固定 `sequential` |
| `tasks` | 按执行顺序排列的完整阶段列表；前端最多接受 200 个 |
| `metrics` | 运行级通用标量元数据，不参与默认进度/ETA 算法 |
| `error` | 无错误为 `null`，否则为非空 `code`、`message`；run 为 failed 时必须有错误 |

run、attempt 和 task ID 使用字母或数字开头，后续仅字母、数字、点、下划线、短横线，长度 1–128。tasks 非空，ID 唯一；同 attempt 的 task ID 和顺序稳定。`metrics`、`dimensions` 值仅为有限数值、字符串、布尔值或 `null`，不能嵌套。不得使用 NaN、Infinity 或 JSON 注释。

每个 task 包含 `id`、`title`、`state`、`progress`、`eta`、`dimensions`、`metrics`。`progress.completed` 是有限非负工作量，`total` 是正数或 `null`，`unit` 为非空单位；total 已知时 completed 不得超过 total。`eta.remaining_seconds` 为有限非负秒数或 `null`，`eta.method` 为 `reported`、`rate`、`historical`、`manual`、`unknown`；未知值与 unknown 方法必须同时出现。`eta.updated_at` 始终为 UTC 时间，表示估计最后一次产生或明确确认的时间。

## 串行与生命周期

同一时刻至多一个 running 阶段；前面的阶段 succeeded 后才能启动下一阶段。pending 阶段不推进工作量，但可以报告未来阶段预算。completed 同 attempt 单调不减，终态不能恢复 running；run 的 succeeded 需源端确认所有阶段完成及结果保存。失败、取消不会伪装成功，不再显示有效剩余时间。重跑更换 attempt，seq 重新从 1 起，允许进度归零；不能只靠日志截断来表达重跑。

同 attempt 相同 seq 相同内容视为重复观测；同轮窗口内的同 seq 内容冲突，以及与已保存最新 seq 的内容冲突均报错。旧 seq 和已退役 attempt 的 tail 重放不替换当前快照。新记录须通过时间、任务集合和生命周期校验；新 attempt 的 started_at 和 timestamp 不得早于当前 attempt。checkpoint 保存当前 attempt、最后快照及各 attempt 的 seq/hash，最多跟踪 128 个 attempt；恢复后先标记未采集，再由轮询确认可用性。

## 工作量与 ETA

总工作量与剩余耗时是独立量。只有各 task 的 unit 完全相同且 total 全部已知时，才计算 `sum(completed) / sum(total)`。不同 unit 不混加，未知总量用 `null`，不拿 0 或猜测分母替代。已知 total 后，圆环按各阶段 `total / sum(total)` 分段，各段内按 `completed / total` 填充；不按阶段数或 ETA 分配圆弧。当前示例三段 total 都是 50，因此恰好等分。

串行总 ETA 是全部未完成阶段 remaining_seconds 之和，要求每个预算都已知且有效；不能跳过未知阶段只给部分总和。pending 阶段 ETA 是该阶段自己的预算，不包含等待前置阶段的时间。succeeded 阶段确认 ETA 为 0。整体 succeeded 后显示完成；failed/cancelled 显示停止。阶段完成比例不等于耗时完成比例，不由前端外推。

## 两个时间轴

心跳只更新 `timestamp`、`seq`，反映源进程仍在报告；不能自动更新 `eta.updated_at`，也不能用采集时间掩盖旧源数据。工作量未变不代表心跳失效。ETA 只有重算或源端明确确认时才更新时间；普通 Python 记录器心跳刻意保留其旧时间。

前端默认源快照过期阈值和 ETA 有效期各为 120 秒，独立判断。有效 ETA 作为快照预算展示，不做本地倒计时。断连、快照过期或超过 5 秒的未来源时间异常时保留最后观测并标记冻结；冻结预算不是有效的新预测。源心跳仍新鲜但 ETA 过期时，ETA 为未知。

合成 demo 例外地每 30 秒明确重申固定预算：completed 仍为 50/17/0，remaining_seconds 仍为 0/480/900；这只是固定演示，不声称实测或真实推断。提交的固定时间日志会过期，运行 `record.py` 才会产生新鲜合成心跳。

## 读入、半行与坏行

只处理以换行结束的完整行。每轮重新打开 source，重读末尾 256 KiB 有界窗口，另读 1 字节用于识别窗口截断；若超出窗口，丢弃可能截断的首行。末尾未换行的半行本轮忽略，下一轮重新读取窗口，不维护读取 offset 或残留半行，不跨文件拼接。

完整但 JSON/Schema/生命周期无效的行不会替换最后有效快照，采集器继续扫描后续完整行。最后一个完整行的校验结果决定本轮 collection 健康状态；若没有完整记录则为 `not_ready`。坏行后还有有效行可恢复 `ok`，有效行后出现坏行则保留数据并标记 `unreachable`。窗口以外的日志不会被补采，不提供历史恢复保证。

checkpoint 原子写入临时 JSON 后替换，默认位于 `data/`；保存最后快照和 attempt/seq 去重状态，不是增量文件 cursor 或历史数据库。文件轮转、截断或替换后仍重新读取窗口并进行快照校验。缓存时间不替代源心跳时间，前端不解析日志。

## 后端配置、API 与 manifest

Go module 为 `eta-bar`，要求 Go 1.25；启动命令为 `go run ./cmd/eta-server -config config.json`。Config JSON 仅包含以下结构：

```json
{
  "hosts": {
    "demo-host": {"alias": "experiment-host", "allowed_root": "/data/experiments"}
  },
  "runs": [{
    "id": "demo-run",
    "title": "Synthetic demo",
    "source": {"mode": "local", "log_path": "/data/experiments/demo/monitor.jsonl"}
  }]
}
```

`source.mode` 仅为 `local` 或 `ssh`，`log_path` 必须是绝对路径。SSH 另设 `source.host` 为 profile key（例中 `demo-host`），通过其 alias 连接，并将真实解析后的远端路径约束在 allowed_root 内。run 可附 `manifest` JSON 对象。未知字段由 `LoadConfig` 的 `DisallowUnknownFields` 拒绝，包括 `listen`、`data_dir`、`poll_interval_seconds`、`sources`。

配置文件、监听和缓存目录通过 `-config`、`-listen`、`-data-dir` flag 指定；默认从 `ETA_CONFIG`、`ETA_LISTEN`、`ETA_DATA_DIR` 读取，未设置时分别为 `config.json`、`127.0.0.1:8080`、`data`。显式 flag 优先。`ETA_API_TOKEN` 设置 Bearer token。轮询固定 15 秒，启动时立即采集，API 请求只读缓存，不触发 SSH 或实验执行。

| GET 路由 | 响应 |
| --- | --- |
| `/healthz` | `{"status":"ok"}`，无需鉴权 |
| `/v1/runs` | `{"runs":[{"id":"demo-run","title":"Synthetic demo"}]}` |
| `/v1/runs/{id}/snapshot` | `data` 与 `collection` envelope，见[前端说明](../frontend/README.md) |
| `/v1/runs/{id}/manifest` | 配置中的 manifest 对象；未配置时返回默认对象 |
| `/v1/runs/{id}/history` | 不支持；鉴权通过后返回 404 |

设置 token 后除 healthz 外需 `Authorization: Bearer …`，未通过时返回 401。未知 run 或路由返回 404，非 GET 返回 405。采集层错误与实验错误分别位于 `collection.error` 和 `data.error`。响应不公开原始日志行或任意源错误正文。

manifest 原样返回配置的对象；应只放稳定的运行/阶段标识、标题、执行顺序、progress unit 与指标单位/含义，不能包含 SSH 目标、源路径、凭据、账号、私有业务或启动命令。未配置时返回：

```json
{"schema_version":"1.0","run_id":"demo-run","title":"Synthetic demo","execution":"sequential","tasks":[]}
```

manifest 不用于推算未知预算或 total。原生 bar 负责校验 envelope、显示状态、绘图及每 30 秒刷新，不连接 SSH，不读取实验目录。

## 安全与分发

SSH 连接信息、私钥、API token 由部署环境管理，不放源码、manifest、样例、错误正文或 Git 仓库；启用 known_hosts 校验。无 token 服务只允许 loopback；非 loopback 监听必须设置 `ETA_API_TOKEN`。Compose 强制要求 token，主机端口绑定 `127.0.0.1:8080`，容器内监听 `0.0.0.0:8080`。运行镜像为 Alpine 3.22，包含 OpenSSH client，使用 UID 10001。配置和日志只读挂载，缓存使用 `/data` volume；本轮仅做本地联调。

示例 `experiment-host` 为 SSH Host 别名，`compute.example.org` 为通用域名，`/data/experiments/demo/monitor.jsonl` 为通用占位路径。真实部署配置与日志均留在版本控制之外。MIT 只覆盖本仓库原创代码；设计参考只链接公开 [Metrix.Bar 文档](https://metrix.bar/docs/bar-format.md) 和 [组件文档](https://metrix.bar/docs/components.md)，不复制官方实现或 skill。
