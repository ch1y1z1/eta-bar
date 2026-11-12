# ETA Bar

用 Go 只读采集 JSONL 实验进度，在原生 Metrix.Bar 面板显示串行阶段进度与剩余时间。前端不需要网页、npm 或浏览器服务。仓库示例完全合成，不代表真实实验。

默认离线 demo：三阶段各 50 step，完成量为 50、17、0；整体 **67/150 ≈ 45%**，剩余预算 **23 分钟**。圆环按各阶段 total 分段，只有一个刷新按钮；前端每 30 秒刷新，刷新不推进工作量。

## 项目结构

- `cmd/eta-server`、`internal/backend`：Go 标准库 HTTP 服务、local/SSH 采集器及测试；module 为 `eta-bar`，Go 1.25。
- `frontend/eta.bar`：[原生面板](frontend/README.md)，ID `eta-sequential-prototype`，版本 `0.2.1`。
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
Path("config.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
PY
go run ./cmd/eta-server -config config.json
```

默认监听 `127.0.0.1:8080`，缓存写入 `data/`。另一个终端可以用 `curl http://127.0.0.1:8080/healthz`、`curl http://127.0.0.1:8080/v1/runs` 和 `curl http://127.0.0.1:8080/v1/runs/demo-run/snapshot` 查看结果。启动时立即采集，之后固定每 15 秒轮询。

`--once` 仅写一条后退出，源时间超过 120 秒会被面板标记过期。要持续观察，先在单独终端运行不带 `--once` 的生成命令，再生成配置并启动服务。脚本每 30 秒续写完整快照，Ctrl+C 退出；输出必须是新文件，重复执行时换文件名，并同步配置中的路径。每次启动使用新 attempt，同次执行 seq 递增，进度与预算固定。提交的 `monitor.jsonl` 使用固定时间 `2026-01-01T00:00:00.000Z`，仅用于格式示例。

运行配置包含本机绝对路径，留在本地；不要提交生成的 `config.json`、日志和缓存。[示例说明](examples/README.md) 包含 SSH profile 和任务接入用法。

## 配置与 API

Config JSON 顶层仅有 `hosts` 和 `runs`。每个 run 包含 `id`、`title`、`source`，可选 `manifest` 对象；source 为 `mode: local|ssh`、绝对 `log_path`，SSH 另需 `host` 指向 hosts 的 profile key。`LoadConfig` 使用 `DisallowUnknownFields`，会拒绝 `listen`、`data_dir`、`poll_interval_seconds`、`sources` 等字段。

| 命令 flag | 环境变量 | 默认值 |
| --- | --- | --- |
| `-config` | `ETA_CONFIG` | `config.json` |
| `-listen` | `ETA_LISTEN` | `127.0.0.1:8080` |
| `-data-dir` | `ETA_DATA_DIR` | `data` |

显式 flag 优先于环境变量。Bearer token 通过 `ETA_API_TOKEN` 设置；未设置时仅允许 loopback 监听，设置后除 `/healthz` 外的 API 需要 `Authorization: Bearer …`。

已实现 GET `/healthz`、`/v1/runs`、`/v1/runs/{id}/snapshot` 和 `/v1/runs/{id}/manifest`。history 不受支持，鉴权通过后的 `/v1/runs/{id}/history` 返回 404。每轮重读文件末尾 256 KiB 有界窗口（另读 1 字节判断首行截断），不保存文件增量 cursor，也不提供历史数据库。checkpoint 原子写入缓存目录，用于恢复最后快照和 attempt/seq 去重状态。

## Docker 本地运行

镜像使用 `golang:1.25-alpine` 构建，运行层为 Alpine 3.22，包含 OpenSSH client，进程 UID/GID 为 `10001:10001`。需要 Docker Compose 和 OpenSSL。在仓库根目录先生成容器挂载用日志和配置（会替换本地 `config.json`，切回本地 Go 时重新运行上面的配置生成片段）：

```sh
python3 examples/demo/record.py demo-data/monitor.jsonl --once
python3 - <<'PY'
import json
from pathlib import Path

config = json.loads(Path("examples/config.example.json").read_text(encoding="utf-8"))
config["runs"][0]["source"]["log_path"] = "/logs/monitor.jsonl"
Path("config.json").write_text(json.dumps(config, indent=2) + "\n", encoding="utf-8")
PY
export ETA_API_TOKEN=$(openssl rand -hex 32)
docker compose up --build
```

持续心跳同样可去掉 `--once` 并保持生成脚本运行，输出需为新文件。不要打印或提交 token；将同一值填入面板的 secret 输入。Compose 强制要求 `ETA_API_TOKEN` 非空，主机仅发布 `127.0.0.1:8080`，容器监听 `0.0.0.0:8080`。本地 Go 服务和容器使用同一端口时应先停止前者。

挂载为 `config.json:/config/config.json:ro`、`demo-data:/logs:ro` 和命名缓存卷 `eta-cache:/data`。SSH 可选挂载 `ssh:/home/eta/.ssh:ro`；专用 config、key、known_hosts 需允许 UID 10001 读取，私钥权限为 0600。不要关闭主机校验。结束后运行 `docker compose down`，缓存卷保留。本轮运行范围为本地联调。

## Metrix.Bar 导入

在 Metrix.Bar 中导入 `frontend/eta.bar`。默认 `demo` 无需网络，可核对 45%、67/150、23 分钟和三段圆环。文件与既有原型使用相同 ID，导入时留意应用对已有实例的提示。

接入后端时设置 `mode=api`，`baseURL=http://127.0.0.1:8080`（纯 origin）、`runID=demo-run`；服务端设置 token 时，在 secret 类型的 `token` 输入内填入同一值。不要将 token 写进 URL 或配置样例。原生应用导入验证需在 Metrix.Bar 内完成；本地 HTTP 联调不替代应用验证。

## 安全范围与限制

只发布原创代码、通用协议和合成样例。真实日志、主机配置、私钥、token、账号、会话链接、个人目录及实验身份信息均不属于仓库内容。`experiment-host` 与 `/data/experiments/demo/monitor.jsonl` 是通用占位值。SSH 凭据及 known_hosts 由运行环境管理。

仅支持串行任务；跨 unit 不汇总工作量，total 未知时不绘制整体圆环。ETA 依赖源端显式报告，不按进度比例推算或本地倒计时。源心跳与 ETA 各有独立有效期，默认 120 秒；断连或过期保留最后快照并冻结。记录器普通心跳不刷新 ETA 时间，合成 demo 明确重申固定预算。

## 许可

原创代码采用 [MIT](LICENSE)。Metrix.Bar 应用和官方资料遵循各自许可；本仓库只链接公开[格式文档](https://metrix.bar/docs/bar-format.md)与[组件文档](https://metrix.bar/docs/components.md)，不复制其实现或 skill。
