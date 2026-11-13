# ETA Bar

用 Go 只读采集 JSONL 实验进度，在原生 Metrix.Bar 面板显示串行阶段进度与剩余时间。前端不需要网页、npm 或浏览器服务。仓库示例完全合成，不代表真实实验。

默认离线 demo：三阶段各 50 step，完成量为 50、17、0；整体 **67/150 ≈ 45%**，剩余预算 **23 分钟**。圆环按各阶段 total 分段，只有一个刷新按钮；前端每 30 秒刷新，刷新不推进工作量。阶段列表下方的原生 ETA 曲线按真实观测时间绘制，并在 Mac 本机保留有界历史；默认 demo 的固定预算形成水平线。

## 项目结构

- `cmd/eta-server`、`internal/backend`：Go 标准库 HTTP 服务、local/SSH 采集器及测试；module 为 `eta-bar`，Go 1.25。
- `frontend/eta.bar`：[原生面板](frontend/README.md)，ID `eta-sequential-prototype`，版本 `0.3.0`。
- `schema/snapshot.schema.json`、[协议说明](docs/protocol.md)：快照格式、采集及 API 契约。
- `examples/demo/monitor.jsonl`：固定 UTC 时间的合成快照。
- `examples/demo/record.py`：生成新鲜合成心跳，支持 `--once`；`write_log.py` 保留同样入口。
- `examples/python/monitor.py`：供自己的串行任务使用的 Python 标准库记录器。
- `examples/config.example.json`、`examples/config.ssh.example.json`：local 和 SSH 配置，均使用通用占位值，使用前替换路径。
- `Dockerfile`、`compose.yaml`：本地容器运行配置。

## 本地 quickstart

需要 Go 1.25 和 Python 3；在仓库根目录执行。先生成一条新鲜合成日志，再用 Python 标准库生成包含绝对路径的运行配置：

```sh
python3 examples/demo/record.py examples/demo/live-monitor.jsonl --once
python3 - <<'PY'
import json
from pathlib import Path

config = json.loads(Path("examples/config.example.json").read_text(encoding="utf-8"))
config["runs"][0]["source"]["log_path"] = str(Path("examples/demo/live-monitor.jsonl").resolve(strict=True))
config["local_allowed_roots"] = [str(Path("examples/demo").resolve(strict=True))]
Path("config.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
PY
go run ./cmd/eta-server -config config.json
```

默认监听 `127.0.0.1:8080`，缓存写入 `data/`。另一个终端可以用 `curl http://127.0.0.1:8080/healthz`、`curl http://127.0.0.1:8080/v1/runs` 和 `curl http://127.0.0.1:8080/v1/runs/demo-run/snapshot` 查看结果。启动时立即采集，之后固定每 15 秒轮询。

`--once` 仅写一条后退出，源时间超过 120 秒会被面板标记过期。要持续观察，先在单独终端运行不带 `--once` 的生成命令，再生成配置并启动服务。脚本每 30 秒续写完整快照，Ctrl+C 退出；输出必须是新文件，重复执行时换文件名，并同步配置中的路径。每次启动使用新 attempt，同次执行 seq 递增，进度与预算固定。提交的 `monitor.jsonl` 使用固定时间 `2026-01-01T00:00:00.000Z`，仅用于格式示例。

运行配置包含本机绝对路径，留在本地；不要提交生成的 `config.json`、日志和缓存。[示例说明](examples/README.md) 包含 SSH profile 和任务接入用法。

## 配置与 API

Config JSON 顶层为 `hosts`、`runs` 和可选 `local_allowed_roots`（绝对目录数组，默认空；禁止 `/`）。`local_allowed_roots` 仅授权路径 API 的 local 读取；既有 `runs` 中明确配置的单个 local 文件保持原有授权。`runs` 可为空，路径 API 无需逐个登记文件。每个 run 包含 `id`、`title`、`source`，可选 `manifest` 对象；source 为 `mode: local|ssh`、绝对 `log_path`，SSH 另需 `host` 指向 hosts 的 profile key。`LoadConfig` 使用 `DisallowUnknownFields`，会拒绝 `listen`、`data_dir`、`poll_interval_seconds`、`sources` 等字段。

| 命令 flag | 环境变量 | 默认值 |
| --- | --- | --- |
| `-config` | `ETA_CONFIG` | `config.json` |
| `-listen` | `ETA_LISTEN` | `127.0.0.1:8080` |
| `-data-dir` | `ETA_DATA_DIR` | `data` |

显式 flag 优先于环境变量。Bearer token 通过 `ETA_API_TOKEN` 设置；未设置时仅允许 loopback 监听，设置后除 `/healthz` 外的 API 需要 `Authorization: Bearer …`。

已实现 GET `/healthz`、`/v1/runs`、`/v1/runs/{id}/snapshot` 和 `/v1/runs/{id}/manifest`。history 不受支持，鉴权通过后的 `/v1/runs/{id}/history` 返回 404。每轮重读文件末尾 256 KiB 有界窗口（另读 1 字节判断首行截断），不保存文件增量 cursor，也不提供历史数据库。checkpoint 原子写入缓存目录，用于恢复最后快照和 attempt/seq 去重状态。

路径模式使用 `GET /v1/snapshot?host=local&path=%2Flogs%2Fmonitor.jsonl`，始终要求配置 token 并携带 Bearer 鉴权（即使 loopback）。`host` 必须是 `local` 或预设 `hosts` key；`path` 为该主机上的绝对路径，不能包含 `..` 路径段，必须位于允许根目录内部。仅接受恰好一个 `host` 和一个 `path`，不接受额外查询参数。未知主机、非法路径和未授权根统一返回 400 `invalid_source`，不会回显路径。根内文件不存在或读取失败返回 200 envelope，`collection.state=unreachable`，可带该源的最后有效数据。

每次路径请求现场读取同样的有界 JSONL 窗口，自动从首条完整有效快照确定 `run_id`，复用生命周期校验。单个路径在缓存存活期间固定 run ID；重跑请更换 attempt ID。缓存按规范化 host/path 隔离，最多 64 个源，LRU 淘汰，单源最多 128 个 attempt；不增加后台 watcher 或磁盘 checkpoint。重启或淘汰会清除该源生命周期历史，只能从当前窗口重新验证。请求串行采集；SSH 单次最多 8 秒。local 使用 Go `os.Root` 限制文件访问，阻止符号链接逃逸；SSH 在预设 alias 上用固定命令解析真实路径并验证根目录。根目录与 SSH 环境由部署者管理，不能让不可信进程在远端校验和读取之间重定向文件。

## Docker / WSL 直接连接

镜像使用 `golang:1.25-alpine` 构建，运行层为 Alpine 3.22，包含 OpenSSH client，进程 UID/GID 为 `10001:10001`。需要 Docker Compose 和 OpenSSL。在仓库根目录先生成容器挂载用日志和配置（会替换本地 `config.json`，切回本地 Go 时重新运行上面的配置生成片段）：

```sh
python3 examples/demo/record.py demo-data/monitor.jsonl --once
python3 - <<'PY'
import json
from pathlib import Path

config = json.loads(Path("examples/config.example.json").read_text(encoding="utf-8"))
config["runs"][0]["source"]["log_path"] = "/logs/monitor.jsonl"
config["local_allowed_roots"] = ["/logs"]
Path("config.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
PY
export ETA_API_TOKEN=$(openssl rand -hex 32)
docker compose up --build
```

持续心跳同样可去掉 `--once` 并保持生成脚本运行，输出需为新文件。不要打印或提交 token；将同一值填入面板的 secret 输入。Compose 强制要求 `ETA_API_TOKEN` 非空，主机默认发布 `0.0.0.0:8080`，容器监听 `0.0.0.0:8080`。如需覆盖发布地址，启动前设置 `ETA_PUBLISH_ADDRESS`（例如 `127.0.0.1`）；这与容器内的 `ETA_LISTEN` 独立。WSL 部署需要 Mac 到 WSL 主机端口的路由和防火墙放行；Mac 直接请求后端地址，无需 SSH 隧道。HTTP Bearer 依赖可信网络，跨不可信网络应在后端前提供 HTTPS。本地 Go 服务和容器使用同一端口时应先停止前者。

挂载为 `config.json:/config/config.json:ro`、`demo-data:/logs:ro` 和命名缓存卷 `eta-cache:/data`。SSH 可选挂载 `ssh:/home/eta/.ssh:ro`；专用 config、key、known_hosts 需允许 UID 10001 读取，私钥权限为 0600。不要关闭主机校验。结束后运行 `docker compose down`，缓存卷保留。日志路径填写容器内路径；要读取其他实验目录，先以只读方式挂载该目录并在 `local_allowed_roots` 中授权一次，此后切换目录内文件只需修改 bar 的路径。

## Metrix.Bar 导入

在 Metrix.Bar 中导入 `frontend/eta.bar`。默认 `demo` 无需网络，可核对 45%、67/150、23 分钟和三段圆环。文件与既有原型使用相同 ID，导入时留意应用对已有实例的提示。

Mac 原生应用接入 WSL 时设置 `mode=api`，`baseURL` 为 Mac 可达的后端纯 origin（如 `http://backend.example.org:8080`），`sourceHost=local`、`logPath=/logs/monitor.jsonl`，在 secret 类型的 `token` 输入内填入 `ETA_API_TOKEN` 的同一值。其他日志只修改 `logPath`；SSH 日志选择预设 `sourceHost`，无需修改 run ID 或逐次编辑后端配置。`logPath` 留空时保留 `runID=demo-run` 旧路由。公开源码默认 origin 为通用的 `http://localhost:8080`，部署时在应用输入中替换。网络权限的 `allow-http=true` 支持非 loopback HTTP；protocol 0 使用 `Text.textColor` 和 `$metrix.secret('token')`，兼容 Metrix.Bar 0.1.2。不要将 token 写进 URL 或配置样例。原生应用导入验证需在 Metrix.Bar 内完成；本地 HTTP 联调不替代应用验证。

## 安全范围与限制

只发布原创代码、通用协议和合成样例。真实日志、主机配置、私钥、token、账号、会话链接、个人目录及实验身份信息均不属于仓库内容。`experiment-host` 与 `/data/experiments/demo/monitor.jsonl` 是通用占位值。SSH 凭据及 known_hosts 由运行环境管理。

仅支持串行任务；跨 unit 不汇总工作量，total 未知时不绘制整体圆环。ETA 依赖源端显式报告，不按进度比例推算或本地倒计时。源心跳与 ETA 各有独立有效期，默认 120 秒；断连或过期保留最后快照并冻结。记录器普通心跳不刷新 ETA 时间，合成 demo 明确重申固定预算。

前端 0.3.0 通过声明 `apis="localStorage"` 使用 dashboard 本地 localStorage 保存最近 24 小时 ETA 采样；每尝试最多 720 点、每源最多 8 个 run/attempt、最多 8 源，并限制序列化容量。源及 run/attempt 隔离，旧请求不能覆盖新源历史。失效观测及超过 120 秒的观测空窗断线，不以冻结 ETA 补点；仅确认成功追加一次零。存储失败显示会话状态且不影响实时进度。历史仅来自应用实际观测，不能回填离线期间；详细规则和运行时契约见 [frontend README](frontend/README.md#本机-eta-历史030)。后端及 API 不变。

## 验证

```sh
go test -race ./...
go vet ./...
go build -o /tmp/eta-server ./cmd/eta-server
node --test frontend/eta.test.mjs
```

Node 测试使用内置 test/vm 模块，无需 npm；检查路径请求、原生 secret、旧路由、demo、ETA 时间/值去重、真实时间横轴、未知/过期/断连断线、终态零值、源与 attempt 隔离、Worker 重载、损坏/不可用/超额存储、有界保留及 A→B→A 异步请求隔离。Go 测试覆盖根授权、符号链接逃逸、SSH 固定脚本、生命周期、失败缓存隔离及 LRU 上限。Metrix.Bar 应用内解析与运行检查由安装环境执行。

## 许可

原创代码采用 [MIT](LICENSE)。Metrix.Bar 应用和官方资料遵循各自许可；本仓库只链接公开[格式文档](https://metrix.bar/docs/bar-format.md)与[组件文档](https://metrix.bar/docs/components.md)，不复制其实现或 skill。
