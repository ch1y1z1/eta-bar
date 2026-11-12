# 通用示例

本目录仅含合成日志和通用配置占位值。`config.example.json` 使用 local source，run ID 为 `demo-run`，绝对路径占位为 `/data/experiments/demo/monitor.jsonl`；复制后替换成自己的绝对路径，或按[根目录 quickstart](../README.md)用 Python `Path.resolve()` 生成本地 `config.json`。运行配置和实际日志不提交。

Config JSON 仅接受 `hosts` 和 `runs`；run 为 `id`、`title`、`source` 和可选对象 `manifest`。source 为 `mode`、绝对 `log_path` 和 SSH 可选字段 `host`。监听、缓存通过 `-listen`/`ETA_LISTEN`、`-data-dir`/`ETA_DATA_DIR` 设置，配置文件通过 `-config`/`ETA_CONFIG` 设置；默认分别为 `127.0.0.1:8080`、`data`、`config.json`。token 仅通过 `ETA_API_TOKEN` 设置。未知配置字段会被 `DisallowUnknownFields` 拒绝。

## SSH profile 示例

`config.ssh.example.json` 中 `source.host=demo-host` 指向 `hosts` 的 key；该 profile 的 `alias=experiment-host` 才是传给 OpenSSH 的 Host 别名，`allowed_root=/data/experiments` 为远端允许读取的根目录。路径仍为 `/data/experiments/demo/monitor.jsonl`。使用前自行配置专用 SSH config、key 和 known_hosts；真实值留在本地，不写入示例。

采集使用非交互 SSH、严格主机校验、5 秒连接超时及 8 秒采集上下文超时，远端用 `readlink -f` 校验路径位于 allowed_root 下再执行有界 tail。配置示例本身不发起连接；本轮验证只使用 local source。Docker 日志路径应为容器内 `/logs/monitor.jsonl`，生成及挂载步骤见根目录 README。

## 合成日志

仓库根目录运行：

```sh
python3 examples/demo/record.py examples/demo/live-monitor.jsonl
```

立即写入后每 30 秒续写，固定进度 50/17/0、固定预算 0/480/900 秒，`run_id=demo-run`。每次启动生成新的 attempt ID，同一次执行 seq 递增。用新文件名重启，脚本拒绝覆盖现有文件；`--once` 只写一条。合成心跳明确重新确认预算，不模拟真实实验，也不计算速度。停止后日志会过期。

`demo/monitor.jsonl` 是固定 UTC 时间样本，用于阅读和格式验证；要演示健康观测请用生成脚本。生成的 live 文件不属于发布样例，勿提交实际部署日志。

## 接入自己的任务

`python/monitor.py` 是标准库记录器模块，不是可直接启动的实验程序。以下片段假定它位于 Python 导入路径，`output_directory` 为调用方选择的新目录：

```python
from monitor import Monitor

with Monitor(output_directory, "example-run", "attempt-001", [
    {"id": "prepare", "title": "准备", "total": 2, "unit": "step"},
]) as monitor:
    monitor.start_task("prepare")
    monitor.update_task("prepare", completed=1, eta_seconds=10)
    # 在调用方完成实际工作后报告进度。
    monitor.update_task("prepare", completed=2, eta_seconds=0)
    monitor.finish_task("prepare")
```

调用方提供实际工作与估计；记录器不推断 ETA。默认后台心跳为 30 秒，只更新 seq 和源 timestamp，不刷新 ETA 时间。每次变更写完整 `monitor.jsonl`，并原子替换旁路 `status.json`；输出日志采用独占创建，避免混合 attempt。上下文提前结束或异常会记录 failed，正常退出要求各阶段已完成。错误信息采用通用文本，不收集 traceback；调用方仍需审查自己传入的 title、metrics、dimensions 等字段，不能将身份或凭据写入日志。
