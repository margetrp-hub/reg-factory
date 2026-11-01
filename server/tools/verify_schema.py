#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""校验 scripts.schema.json 与各脚本真实 CLI 是否一致。

背景：`webui/scripts.py` 是人工维护的 schema，和 argparse 定义会漂移
（例如 export_accounts 的 platforms 是位置参数，schema 却写成 --platform，
旧面板和新面板都会把参数拼错）。构建前跑一遍，把漂移暴露出来。

检测方式用**源码静态扫描** add_argument(...) 而不是 `--help`：
很多脚本用 help=argparse.SUPPRESS 把选项藏起来，--help 看不到会误报。

用法：
    python server/tools/verify_schema.py [schema.json]

退出码：0 = 全部一致；1 = 存在漂移（打印明细）
"""
from __future__ import annotations

import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
ADD_BLOCK_RE = re.compile(r"add_argument\(([^)]*)\)", re.S)
LONG_FLAG_RE = re.compile(r"[\"'](--[A-Za-z0-9][\w-]*)[\"']")
SYS_ARGV_RE = re.compile(r"sys\.argv\[1|sys\.argv\s*\[1:")
POSITIONAL_RE = re.compile(r"add_argument\(\s*[\"']([A-Za-z_][\w-]*)[\"']")


def declared_flags(script: Path) -> tuple[set[str], set[str], bool]:
    """返回 (长选项集合, 位置参数集合, 是否用 sys.argv 直接取值)。"""
    text = script.read_text(encoding="utf-8", errors="replace")
    flags: set[str] = set()
    for block in ADD_BLOCK_RE.findall(text):
        flags.update(LONG_FLAG_RE.findall(block))
    positionals = set(POSITIONAL_RE.findall(text))
    # add_argument("-o", "--out") 这类，长选项已由 ADD_BLOCK_RE 覆盖
    uses_argv = bool(SYS_ARGV_RE.search(text))
    return flags, positionals, uses_argv


def main() -> int:
    schema_path = Path(sys.argv[1]) if len(sys.argv) > 1 else ROOT / "deploy" / "scripts.schema.json"
    if not schema_path.is_file():
        print(f"找不到 schema: {schema_path}", file=sys.stderr)
        return 1
    schema = json.loads(schema_path.read_text(encoding="utf-8"))

    drift = 0
    for item in schema:
        script = ROOT / item["file"]
        if not script.is_file():
            print(f"[缺失脚本] {item['id']}: {item['file']}")
            drift += 1
            continue
        flags, positionals, uses_argv = declared_flags(script)

        missing_flags = []
        missing_positionals = []
        for arg in item.get("args", []):
            flag = arg["flag"]
            if arg.get("positional") or not flag.startswith("-"):
                name = flag.lstrip("-")
                if uses_argv or name in positionals or "--" + name in flags:
                    continue
                missing_positionals.append(name)
                continue
            if flag not in flags:
                missing_flags.append(flag)

        # 反向：CLI 有、schema 没暴露（低频参数可以接受，只提示）
        schema_flags = {
            arg["flag"] for arg in item.get("args", []) if arg["flag"].startswith("-")
        }
        hidden = sorted(flag for flag in flags if flag not in schema_flags and flag != "--help")

        if missing_flags or missing_positionals:
            drift += 1
            detail = []
            if missing_flags:
                detail.append("schema 有但 CLI 无: " + ", ".join(sorted(missing_flags)))
            if missing_positionals:
                detail.append("位置参数不存在: " + ", ".join(sorted(missing_positionals)))
            print(f"[漂移] {item['id']:24s} " + " | ".join(detail))
        elif hidden:
            print(f"[提示] {item['id']:24s} CLI 有但 schema 未暴露: {', '.join(hidden[:8])}")

    print(f"\n检查 {len(schema)} 个任务，发现问题 {drift} 个")
    return 1 if drift else 0


if __name__ == "__main__":
    raise SystemExit(main())
