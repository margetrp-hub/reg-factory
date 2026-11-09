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

def main() -> int:
    out_path = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "deploy" / "scripts.schema.json"
    # schema/scripts.py 是唯一任务定义来源；旧 webui 面板已经退役，
    # 不再静默回退到不存在的旧模块，避免生成“看似成功、实际过期”的 schema。
    from schema import scripts as schema  # noqa: WPS433

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
            args.append(entry)
        exported.append({
            "id": item.get("id", ""),
            "title": item.get("title", ""),
            "file": item.get("file", ""),
            "desc": item.get("desc", ""),
            "platform": item.get("platform", ""),
            "platforms": item.get("platforms", []),
            "group": item.get("group", "other"),
            "category": item.get("category", ""),
            "warning": item.get("warning", ""),
            "args": args,
        })

    out_path.parent.mkdir(parents=True, exist_ok=True)
    out_path.write_text(json.dumps(exported, ensure_ascii=False, indent=2), encoding="utf-8")
    out_path.with_name("config.schema.json").write_text(json.dumps(schema.ENV_SCHEMA, ensure_ascii=False, indent=2), encoding="utf-8")
    # Public navigation metadata contains no runtime values or credentials.
    catalog = {"groups": schema.TASK_GROUPS, "platforms": schema.PLATFORMS, "tools": schema.EXTERNAL_LINKS}
    if len(sys.argv) <= 1:
        (ROOT / "server" / "web" / "lib" / "generated-catalog.json").write_text(json.dumps(catalog, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"导出 {len(exported)} 个任务 -> {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
