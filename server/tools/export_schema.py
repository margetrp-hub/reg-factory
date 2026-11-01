#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""把 Python 侧的任务 schema 导出成 Go 控制面可直接读取的 JSON。

用法（在仓库根或任意目录）：
    python server/tools/export_schema.py [输出路径]

默认输出 <repo>/deploy/scripts.schema.json，构建镜像时一起打进 /app。
"""
from __future__ import annotations

import json
import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

# schema 与真实 CLI 的已知偏差修正（只影响导出，不改上游 Python 面板行为）。
# export_accounts.py 用的是位置参数 `platforms`（nargs="*"），schema 历史写成 --platform，
# 直接透传会让 argparse 报 "invalid choice: []"。
OVERRIDES = {
    ("export_accounts", "--platform"): {"flag": "platforms", "positional": True},
}


def apply_overrides(script_id: str, arg: dict) -> dict:
    patch = OVERRIDES.get((script_id, arg.get("flag")))
    if patch:
        arg.update(patch)
    return arg


def main() -> int:
    out_path = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "deploy" / "scripts.schema.json"
    try:
        from webui import scripts as schema  # noqa: WPS433
    except Exception as exc:  # pragma: no cover
        print(f"导入 webui.scripts 失败: {exc}", file=sys.stderr)
        return 1

    exported = []
    for item in getattr(schema, "SCRIPTS", []):
        args = []
        for arg in item.get("args", []) or []:
            entry = {
                "flag": arg.get("flag", ""),
                "type": arg.get("type", "str"),
                "default": arg.get("default"),
                "help": arg.get("help", ""),
                "choices": arg.get("choices") or [],
                "labels": arg.get("labels") or {},
                "positional": bool(arg.get("positional")),
            }
            args.append(apply_overrides(item.get("id", ""), entry))
        exported.append({
            "id": item.get("id", ""),
            "title": item.get("title", ""),
            "file": item.get("file", ""),
            "desc": item.get("desc", ""),
            "platform": item.get("platform", ""),
            "category": item.get("category", ""),
            "warning": item.get("warning", ""),
            "args": args,
        })

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(exported, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"导出 {len(exported)} 个任务 -> {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
