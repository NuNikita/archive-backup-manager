#!/usr/bin/env python3
"""Exercise the running Compose stack without changing source files."""
import argparse
import hashlib
import http.cookiejar
from html.parser import HTMLParser
import json
import platform
from pathlib import Path
import shutil
import subprocess
import tempfile
import time
import urllib.parse
import urllib.request

ROOT = Path(__file__).resolve().parents[1]


class Forms(HTMLParser):
    def __init__(self):
        super().__init__()
        self.forms = []
        self.current = None

    def handle_starttag(self, tag, attrs):
        attrs = dict(attrs)
        if tag == "form":
            self.current = {"action": attrs.get("action"), "values": {}}
        elif tag == "input" and self.current is not None and attrs.get("name"):
            self.current["values"][attrs["name"]] = attrs.get("value", "")

    def handle_endtag(self, tag):
        if tag == "form" and self.current is not None:
            self.forms.append(self.current)
            self.current = None


def form(page, action):
    parser = Forms()
    parser.feed(page)
    return next(item["values"] for item in parser.forms if item["action"] == action)


def manifest(directory):
    return {
        file.relative_to(directory).as_posix(): hashlib.sha256(file.read_bytes()).hexdigest()
        for file in sorted(directory.rglob("*")) if file.is_file()
    }


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default="http://127.0.0.1:8080")
    parser.add_argument("--docker", default=shutil.which("docker") or "/Applications/Docker.app/Contents/Resources/bin/docker")
    args = parser.parse_args()
    settings = dict(line.split("=", 1) for line in (ROOT / ".env").read_text().splitlines() if line and not line.startswith("#"))
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def request(path, values=None):
        data = urllib.parse.urlencode(values).encode() if values is not None else None
        with opener.open(args.url.rstrip("/") + path, data=data, timeout=15) as response:
            return response.read().decode()

    def docker(*values):
        return subprocess.check_output([args.docker, "compose", *values], cwd=ROOT, text=True)

    def query(sql):
        text = docker("exec", "-T", "db", "psql", "-U", "archive", "-d", "archive", "-Atc", sql).strip()
        return json.loads(text) if text else None

    def operation(kind, values):
        before = query("SELECT coalesce(max(id),0) FROM tasks")
        page = request("/tasks/" + kind, values)
        if "Операция не выполнена" in page:
            raise RuntimeError("Operation rejected by the application")
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            task = query(f"SELECT row_to_json(t) FROM (SELECT id,source,kind,status,snapshot_id,restore_path,log FROM tasks WHERE id>{int(before)} ORDER BY id LIMIT 1) t")
            if task and task["status"] in ("success", "failed"):
                if task["kind"] != kind or task["status"] != "success":
                    raise RuntimeError(json.dumps(task, ensure_ascii=False))
                print(f"{kind}: PASS (task {task['id']})", flush=True)
                return task
            time.sleep(1)
        raise TimeoutError("Operation did not finish within 120 seconds")

    deadline = time.monotonic() + 60
    while True:
        try:
            if request("/healthz").strip() == "ok":
                break
        except OSError:
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError("Application did not become ready within 60 seconds")
        time.sleep(1)
    login = form(request("/login"), "/login")
    login.update(username="admin", password=settings["ADMIN_PASSWORD"])
    dashboard = request("/login", login)
    backup_form = form(dashboard, "/tasks/backup")
    backup = operation("backup", backup_form)
    restore_form = dict(backup_form, snapshot=backup["snapshot_id"])
    restored = operation("restore", restore_form)
    original = ROOT / "demo-data" / backup["source"]
    with tempfile.TemporaryDirectory(prefix="archive-docker-restore-") as temporary:
        target = Path(temporary) / "result"
        docker("cp", "worker:" + restored["restore_path"], str(target))
        candidates = [path for path in target.rglob(original.name) if path.is_dir()]
        if not any(manifest(original) == manifest(path) for path in candidates):
            raise AssertionError("Restored files do not match the source SHA-256 manifest")
    print("restore SHA-256 comparison: PASS", flush=True)
    checked = operation("check", backup_form)
    result = {
        "environment": "Docker Compose",
        "host_os": platform.system(),
        "host_arch": platform.machine(),
        "url": args.url,
        "services": ["proxy", "app", "worker", "db", "storage"],
        "backup_task": backup["id"],
        "snapshot": backup["snapshot_id"],
        "restore_task": restored["id"],
        "sha256_match": True,
        "repository_check_task": checked["id"],
    }
    (ROOT / "docs/docker-verification.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n")
    print("All container checks passed. Evidence: docs/docker-verification.json", flush=True)


if __name__ == "__main__":
    main()
