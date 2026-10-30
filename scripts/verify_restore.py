#!/usr/bin/env python3
"""Compare source and restored directory, including extra files and symlinks."""
import argparse
import hashlib
from pathlib import Path

def manifest(root):
    result = {}
    for path in sorted(root.rglob("*")):
        name = path.relative_to(root).as_posix()
        if path.is_symlink():
            result[name] = ("symlink", str(path.readlink()))
        elif path.is_file():
            digest = hashlib.sha256()
            with path.open("rb") as stream:
                for block in iter(lambda: stream.read(1024 * 1024), b""):
                    digest.update(block)
            result[name] = ("file", digest.hexdigest())
        elif path.is_dir():
            result[name] = ("directory", "")
    return result

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("original", type=Path)
    parser.add_argument("restored", type=Path)
    args = parser.parse_args()
    if not args.original.is_dir() or not args.restored.is_dir():
        raise SystemExit("Оба аргумента должны указывать на существующие папки.")
    before, after = manifest(args.original), manifest(args.restored)
    mismatches = [name for name in sorted(before.keys() | after.keys()) if before.get(name) != after.get(name)]
    for name in mismatches:
        print("Различие:", name)
    if mismatches:
        raise SystemExit(1)
    print(f"Проверка пройдена: {len(before)} объектов совпадают, содержимое файлов проверено по SHA-256.")
