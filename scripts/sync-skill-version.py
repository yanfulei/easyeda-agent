#!/usr/bin/env python3
"""把所有 release 元数据与 skills/easyeda-agent/SKILL.md 同步到发版版本号。

为什么需要它:Agent Skills 规范允许 frontmatter 带 `metadata`(string→string),
我们在里面记 version,让 ClawHub / 目录索引 / 离线拿到 skill 包的人都能看出这份
skill 对应哪个 CLI。根 DSH bundle、MCP、连接器和各自 package-lock 也必须与
release tag 同版；漏掉任意一个都会让源码安装、锁文件安装或 MCP 握手报告旧版本。
所以 release 在构建前统一调用本脚本，并在 CI 发布 skill 前用 --check 复核。

版本号约定:CLI / connector / MCP / DSH bundle / skill 始终同一个版本号。

用法:
    python3 scripts/sync-skill-version.py 1.0.3        # 写入
    python3 scripts/sync-skill-version.py 1.0.3 --check  # 只校验,不一致则非零退出
"""

import argparse
import json
import pathlib
import re
import sys

ROOT = pathlib.Path(__file__).resolve().parent.parent
SKILL = ROOT / "skills" / "easyeda-agent" / "SKILL.md"
JSON_TARGETS = (
    (ROOT / "package.json", 2, False),
    (ROOT / "package-lock.json", 2, True),
    (ROOT / "extension" / "extension.json", "\t", False),
    (ROOT / "extension" / "package.json", "\t", False),
    (ROOT / "extension" / "package-lock.json", 2, True),
    (ROOT / "mcp" / "package.json", 2, False),
    (ROOT / "mcp" / "package-lock.json", 2, True),
)
# 只匹配 frontmatter 里 metadata 块下的 version 行(两空格缩进),不会误伤正文。
PATTERN = re.compile(r'(?m)^(  version:\s*)"([^"]*)"$')


def sync_json(path: pathlib.Path, indent, is_lock: bool,
              want: str, check: bool):
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise RuntimeError(f"无法读取 {path}: {exc}") from exc

    have = data.get("version")
    mismatches = []
    if have != want:
        mismatches.append(f"version={have!r}")
    if is_lock:
        root_package = data.get("packages", {}).get("")
        if not isinstance(root_package, dict):
            raise RuntimeError(f'{path} 缺少 packages[""] 根条目')
        lock_have = root_package.get("version")
        if lock_have != want:
            mismatches.append(f'packages[""].version={lock_have!r}')

    if not mismatches:
        return False, f"  {path.relative_to(ROOT)} 已是 {want}"
    if check:
        return True, (f"error: {path.relative_to(ROOT)} 的 {', '.join(mismatches)},"
                      f"期望 {want}")

    data["version"] = want
    if is_lock:
        data["packages"][""]["version"] = want
        package_path = path.parent / "package.json"
        package_data = json.loads(package_path.read_text(encoding="utf-8"))
        if "license" in package_data:
            data["packages"][""]["license"] = package_data["license"]
    path.write_text(json.dumps(data, ensure_ascii=False, indent=indent) + "\n",
                    encoding="utf-8")
    return False, (f"  {path.relative_to(ROOT)} "
                   f"{have if have is not None else '<missing>'} -> {want}")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("version", help="版本号,不带 v 前缀(如 1.0.3);带了也会被剥掉")
    ap.add_argument("--check", action="store_true", help="只校验是否已同步,不写入")
    args = ap.parse_args()

    want = args.version.lstrip("v")
    if not re.fullmatch(r"\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?", want):
        print(f"error: 非法 SemVer: {args.version!r}", file=sys.stderr)
        return 1
    failed = False
    for path, indent, is_lock in JSON_TARGETS:
        try:
            mismatch, message = sync_json(path, indent, is_lock, want, args.check)
        except RuntimeError as exc:
            print(f"error: {exc}", file=sys.stderr)
            return 1
        print(message, file=sys.stderr if mismatch else sys.stdout)
        failed = failed or mismatch

    if not SKILL.is_file():
        print(f"error: {SKILL} 不存在", file=sys.stderr)
        return 1

    text = SKILL.read_text(encoding="utf-8")
    match = PATTERN.search(text)
    if not match:
        print(f"error: {SKILL} 的 frontmatter 里找不到 metadata.version —— "
              "是不是 frontmatter 被改过?", file=sys.stderr)
        return 1

    have = match.group(2)
    if have == want:
        print(f"  skill version 已是 {want}")
        return 1 if failed else 0

    if args.check:
        print(f"error: skill version 是 {have},期望 {want} —— 跑 "
              f"`python3 scripts/sync-skill-version.py {want}` 同步", file=sys.stderr)
        return 1

    SKILL.write_text(PATTERN.sub(lambda m: f'{m.group(1)}"{want}"', text, count=1), encoding="utf-8")
    print(f"  skill version {have} → {want}")
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
