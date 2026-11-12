"""Synthetic fixed-progress demo, not an experiment or ETA predictor (stdlib only)."""
from __future__ import annotations

import argparse
import copy
import datetime as dt
import json
from pathlib import Path
import threading
import uuid


def utc_now():
    return dt.datetime.now(dt.timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("path", type=Path, help="New output JSONL file; refuses to overwrite")
    parser.add_argument("--once", action="store_true", help="Write one fresh snapshot and exit")
    args = parser.parse_args()
    template = Path(__file__).with_name("monitor.jsonl")
    snapshot = copy.deepcopy(json.loads(template.read_text(encoding="utf-8")))
    snapshot["attempt_id"] = "demo-" + uuid.uuid4().hex
    snapshot["started_at"] = utc_now()
    snapshot["seq"] = 0
    args.path.parent.mkdir(parents=True, exist_ok=True)
    stop = threading.Event()
    # One process owns a new attempt and file; never mix with real experiment logs.
    with args.path.open("x", encoding="utf-8") as output:
        try:
            while True:
                now = utc_now()
                snapshot["seq"] += 1
                snapshot["timestamp"] = now
                # This demo explicitly reconfirms its fixed synthetic budget.
                # Real heartbeats must not renew ETA unless it is recomputed/confirmed.
                for task in snapshot["tasks"]:
                    task["eta"]["updated_at"] = now
                output.write(json.dumps(snapshot, ensure_ascii=False, allow_nan=False,
                                        separators=(",", ":")) + "\n")
                output.flush()
                if args.once or stop.wait(30):
                    return
        except KeyboardInterrupt:
            return


if __name__ == "__main__":
    main()
