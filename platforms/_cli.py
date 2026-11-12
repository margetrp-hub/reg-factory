#!/usr/bin/env python3
"""platforms/_cli.py — dispatcher 的命令行外壳。

给 register_three_platforms.py 这类「exec 子进程」的调用方用，
让它们在不改动执行模型的前提下切到 platforms 层。

输出约定
--------
成功时打印 "success: 1/1"，与原有脚本保持一致，
这样 run_platform 里的成功标记判断无需修改。
"""
from __future__ import annotations

import argparse
import json
import sys


def main() -> int:
    ap = argparse.ArgumentParser(add_help=False)
    ap.add_argument("platform")
    ap.add_argument("--step", default="register")
    ap.add_argument("--email", default="")
    ap.add_argument("--password", default="")
    ap.add_argument("--node", default="auto")
    ap.add_argument("--protocol", default="browser")
    ap.add_argument("--timeout", type=int, default=3600)
    ap.add_argument("--json", action="store_true")
    args, rest = ap.parse_known_args()

    sys.path.insert(0, "/app")
    from platforms import dispatcher

    kwargs = {"email": args.email, "timeout": args.timeout}
    if args.password:
        kwargs["password"] = args.password
    if args.node and args.node != "auto":
        kwargs["node"] = args.node
    if args.protocol and args.protocol != "browser":
        kwargs["protocol"] = args.protocol
    # 其余参数按 --key value 透传
    i = 0
    while i < len(rest):
        token = rest[i]
        if token.startswith("--"):
            key = token[2:].replace("-", "_")
            if i + 1 < len(rest) and not rest[i + 1].startswith("--"):
                kwargs[key] = rest[i + 1]
                i += 2
            else:
                kwargs[key] = True
                i += 1
        else:
            i += 1

    result = dispatcher.run(args.platform, args.step, **kwargs)
    payload = result.as_dict()

    if args.json:
        print(json.dumps(payload, ensure_ascii=False))
    else:
        print(f"[dispatcher] {args.platform}.{args.step} ok={payload['ok']}")
        for k, v in (payload.get("detail") or {}).items():
            if k in ("stdout_tail", "stderr_tail"):
                for line in v or []:
                    print(f"  {line}")
            else:
                print(f"  {k}: {v}")
        if payload.get("error"):
            print(f"  error: {payload['error']}", file=sys.stderr)

    # 兼容 run_platform 的成功标记
    if payload.get("ok"):
        print("success: 1/1")
        return 0
    return 1


if __name__ == "__main__":
    sys.exit(main())
