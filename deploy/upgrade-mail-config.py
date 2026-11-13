"""One-time MAIL_* key rename before upgrading SG; never a runtime fallback.

Run once for every compose env_file and /data/.env used by the deployment.
The original file is backed up; values are never printed. Without --apply,
only changed key names are reported. Shared TURNSTILE_* keys are not removed.
"""
import argparse
import os
from pathlib import Path
import shutil
import tempfile


def upgrade(text):
    lines = text.splitlines(keepends=True)
    values = {}
    for line in lines:
        if "=" in line and not line.lstrip().startswith("#"):
            key, value = line.split("=", 1)
            values.setdefault(key.strip(), value.strip())
    mapping = {key: "MAIL_" + key[7:] for key in values if key.startswith("ICMAIL_")}
    mapping.update({key: target for key, target in {
        "HME_ADMIN_KEY": "MAIL_ADMIN_KEY",
        "REG_FACTORY_ICMAIL_ADDR": "REG_FACTORY_MAIL_ADDR",
        "REG_FACTORY_ICMAIL_KEY": "REG_FACTORY_MAIL_KEY",
    }.items() if key in values})
    replacements = {}
    for old, target in mapping.items():
        candidates = [values.get(target, ""), replacements.get(target, ""), values[old]]
        if old.startswith("ICMAIL_IMAP_"):
            candidates.insert(0, values[old])
        replacements[target] = next((value for value in candidates if value.strip("\"' ")), values[old])
    for suffix in ("SITE_KEY", "SECRET_KEY", "EXPECTED_HOSTNAME"):
        shared, target = "TURNSTILE_" + suffix, "MAIL_TURNSTILE_" + suffix
        if not (replacements.get(target) or values.get(target, "")).strip("\"' ") and values.get(shared, "").strip("\"' "):
            replacements[target] = values[shared]
    if not mapping and not replacements:
        return text, []
    # Preserve comments and unrelated settings, and write each new key once.
    kept = [line for line in lines if line.lstrip().startswith("#") or line.split("=", 1)[0].strip() not in mapping.keys() | replacements.keys()]
    result = "".join(kept).rstrip("\r\n") + "\n"
    result += "".join(f"{key}={value}\n" for key, value in sorted(replacements.items()))
    return result, sorted(replacements)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("file", type=Path)
    parser.add_argument("--apply", action="store_true")
    args = parser.parse_args()
    path = args.file.resolve(strict=True)
    old = path.read_text(encoding="utf-8-sig")
    new, keys = upgrade(old)
    print({"mode": "apply" if args.apply else "dry-run", "keys": keys})
    if args.apply and new != old:
        backup = path.with_name(path.name + ".mail-v1-backup")
        if not backup.exists():
            shutil.copy2(path, backup)
        fd, name = tempfile.mkstemp(prefix=".mail-config-", dir=path.parent)
        with os.fdopen(fd, "w", encoding="utf-8", newline="") as out:
            out.write(new)
        shutil.copystat(path, name)
        if os.name == "posix":
            original = path.stat()
            # Root-run upgrades must leave the config readable by its service user.
            os.chown(name, original.st_uid, original.st_gid)
        os.replace(name, path)


if __name__ == "__main__":
    main()
