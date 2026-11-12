"""Minimal standard-library-only ETA snapshot writer; one process owns one attempt.

Example: see README.md in this directory. Never point this at an existing attempt.
No automatic ETA inference: the experiment explicitly supplies estimates.
"""
from __future__ import annotations

import copy
import datetime as dt
import json
import math
import os
from pathlib import Path
import threading

_UNSET = object()
_TERMINAL = {"succeeded", "failed", "cancelled"}
_METHODS = {"reported", "rate", "historical", "manual", "unknown"}


def utc_now():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def number(value):
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


class Monitor:
    def __init__(self, directory, run_id, attempt_id, tasks, heartbeat_seconds=30):
        if heartbeat_seconds <= 0:
            raise ValueError("heartbeat_seconds must be positive")
        import re
        pattern = r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}"
        if any(not re.fullmatch(pattern, x) for x in (run_id, attempt_id)):
            raise ValueError("invalid run/attempt id")
        now = utc_now()
        prepared = []
        for item in tasks:
            task = copy.deepcopy(item)
            if not re.fullmatch(pattern, task["id"]) or not task["title"] or not task["unit"]:
                raise ValueError("invalid task definition")
            total = task.get("total")
            if total is not None and (not number(total) or total <= 0):
                raise ValueError("task total must be positive or None")
            prepared.append({
                "id": task["id"], "title": task["title"], "state": "pending",
                "progress": {"completed": 0, "total": total, "unit": task["unit"]},
                "eta": {"remaining_seconds": None, "method": "unknown", "updated_at": now},
                "dimensions": task.get("dimensions", {}), "metrics": {},
            })
        if not prepared or len({t["id"] for t in prepared}) != len(prepared):
            raise ValueError("tasks must be nonempty with unique IDs")
        self.data = {"schema_version": "1.0", "run_id": run_id, "attempt_id": attempt_id,
                     "seq": 0, "timestamp": now, "started_at": now, "state": "pending",
                     "execution": "sequential", "tasks": prepared, "metrics": {}, "error": None}
        self.directory = Path(directory)
        self.directory.mkdir(parents=True, exist_ok=True)
        self._lock = threading.RLock()
        self._stop = threading.Event()
        self._writer_error = None
        self._interval = heartbeat_seconds
        self._thread = None
        # Exclusive creation prevents silently appending a new process to an old attempt.
        self._file = (self.directory / "monitor.jsonl").open("x", encoding="utf-8")
        self._tmp = self.directory / "status.json.tmp"
        self._closed = False

    def _emit(self):
        if self._writer_error:
            raise RuntimeError("background log write failed") from self._writer_error
        self.data["seq"] += 1
        self.data["timestamp"] = utc_now()
        record = json.dumps(self.data, ensure_ascii=False, allow_nan=False, separators=(",", ":"))
        if len(record.encode("utf-8")) > 256 * 1024:
            raise ValueError("snapshot exceeds 256 KiB")
        self._file.write(record + "\n")
        self._file.flush()
        self._tmp.write_text(record + "\n", encoding="utf-8")
        os.replace(self._tmp, self.directory / "status.json")
        if self.data["state"] in _TERMINAL:
            os.fsync(self._file.fileno())

    def _heartbeat(self):
        while not self._stop.wait(self._interval):
            try:
                with self._lock:
                    if self.data["state"] in _TERMINAL:
                        return
                    self._emit()  # ETA.updated_at is deliberately unchanged.
            except Exception as error:
                self._writer_error = error
                self._stop.set()
                return

    def _task(self, task_id):
        for task in self.data["tasks"]:
            if task["id"] == task_id:
                return task
        raise KeyError(task_id)

    def _ensure_running(self):
        if self._closed or self.data["state"] != "running":
            raise RuntimeError("attempt is not running")

    def start_task(self, task_id):
        with self._lock:
            self._ensure_running()
            task = self._task(task_id)
            index = self.data["tasks"].index(task)
            if task["state"] != "pending" or any(t["state"] != "succeeded" for t in self.data["tasks"][:index]):
                raise ValueError("sequential tasks must start in order")
            task["state"] = "running"
            self._emit()

    def update_task(self, task_id, *, completed=_UNSET, eta_seconds=_UNSET,
                    eta_method="reported", dimensions=None, metrics=None):
        with self._lock:
            self._ensure_running()
            task = self._task(task_id)
            if task["state"] in _TERMINAL:
                raise ValueError("cannot update terminal task")
            progress = task["progress"]
            if completed is not _UNSET:
                if task["state"] != "running":
                    raise ValueError("pending task cannot advance")
                if not number(completed) or completed < progress["completed"]:
                    raise ValueError("completed must be finite and nondecreasing")
                if progress["total"] is not None and completed > progress["total"]:
                    raise ValueError("completed exceeds total")
                if completed != progress["completed"]:
                    self.data["last_progress_at"] = utc_now()
                progress["completed"] = completed
            if eta_seconds is not _UNSET:
                if eta_method not in _METHODS:
                    raise ValueError("unknown ETA method")
                if eta_seconds is not None and (not number(eta_seconds) or eta_seconds < 0 or eta_method == "unknown"):
                    raise ValueError("ETA must be nonnegative or None")
                task["eta"] = {"remaining_seconds": eta_seconds,
                               "method": "unknown" if eta_seconds is None else eta_method,
                               "updated_at": utc_now()}
            for target, incoming in ((task["dimensions"], dimensions), (task["metrics"], metrics)):
                if incoming:
                    if any(not (v is None or isinstance(v, (str, bool)) or number(v)) for v in incoming.values()):
                        raise ValueError("metrics/dimensions must be scalar and finite")
                    target.update(incoming)
            self._emit()

    def update_metrics(self, **metrics):
        with self._lock:
            self._ensure_running()
            if any(not (v is None or isinstance(v, (str, bool)) or number(v)) for v in metrics.values()):
                raise ValueError("metrics must be scalar and finite")
            self.data["metrics"].update(metrics)
            self._emit()

    def finish_task(self, task_id):
        with self._lock:
            self._ensure_running()
            task = self._task(task_id)
            if task["state"] != "running":
                raise ValueError("only a running task can finish")
            # Caller confirms all work AND final result saving have completed.
            progress = task["progress"]
            if progress["total"] is not None and progress["completed"] != progress["total"]:
                progress["completed"] = progress["total"]
                self.data["last_progress_at"] = utc_now()
            task["state"] = "succeeded"
            task["eta"] = {"remaining_seconds": 0, "method": "reported", "updated_at": utc_now()}
            self._emit()

    def __enter__(self):
        with self._lock:
            if self._thread is not None:
                raise RuntimeError("Monitor can only be entered once")
            self.data["state"] = "running"
            self._emit()
            self._thread = threading.Thread(target=self._heartbeat, daemon=True)
            self._thread.start()
        return self

    def __exit__(self, exc_type, exc, traceback):
        self._stop.set()
        if self._thread:
            self._thread.join(timeout=self._interval + 1)
        unfinished = False
        try:
            with self._lock:
                unfinished = any(t["state"] != "succeeded" for t in self.data["tasks"])
                if exc is not None or unfinished:
                    self.data["state"] = "failed"
                    self.data["error"] = {"code": exc_type.__name__ if exc_type else "INCOMPLETE_TASKS",
                                          "message": "Experiment raised an exception; see stdout.log" if exc else "Context exited before all tasks finished"}
                    for task in self.data["tasks"]:
                        if task["state"] == "running":
                            task["state"] = "failed"
                            task["eta"] = {"remaining_seconds": None, "method": "unknown", "updated_at": utc_now()}
                else:
                    self.data["state"] = "succeeded"
                self._emit()
        finally:
            self._closed = True
            self._file.close()
            self._tmp.unlink(missing_ok=True)
        if exc is None and unfinished:
            raise RuntimeError("experiment context exited with unfinished tasks")
        return False
