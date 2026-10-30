#!/usr/bin/env python3
"""Create local credentials without replacing an existing configuration."""
from pathlib import Path
import secrets

root = Path(__file__).resolve().parents[1]
target = root / ".env"
if target.exists():
    raise SystemExit(".env уже существует. Текущие пароли сохранены.")
values = {
    "POSTGRES_PASSWORD": secrets.token_hex(20),
    "ADMIN_PASSWORD": secrets.token_hex(12),
    "VIEWER_PASSWORD": secrets.token_hex(12),
    "RESTIC_PASSWORD": secrets.token_hex(24),
    "COOKIE_SECURE": "false",
}
with target.open("x") as output:
    target.chmod(0o600)
    output.write("\n".join(f"{key}={value}" for key, value in values.items()) + "\n")
print("Настройки созданы в .env. Учётные записи: admin и viewer.")
print("Откройте .env локально, чтобы посмотреть пароли. Сохраните RESTIC_PASSWORD отдельно.")
