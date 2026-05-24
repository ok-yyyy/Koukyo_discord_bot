from __future__ import annotations

import asyncio
import json
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, Optional


class VandalEventLogger:
    """Append-only JSONL logger with basic daily rotation."""

    def __init__(self, base_dir: Path, rotation: str = "daily") -> None:
        self.base_dir = Path(base_dir)
        self.base_dir.mkdir(parents=True, exist_ok=True)
        self.rotation = rotation
        self._lock = asyncio.Lock()

    async def log(self, event: Dict[str, Any]) -> None:
        """Serialize an event dict into the current JSONL file."""
        ts = self._resolve_timestamp(event.get("timestamp"))
        event["timestamp"] = ts.isoformat().replace("+00:00", "Z")
        line = json.dumps(event, ensure_ascii=False)

        path = self._current_file(ts)
        async with self._lock:
            await asyncio.to_thread(self._append_line, path, line)

    def _resolve_timestamp(self, value: Optional[Any]) -> datetime:
        if isinstance(value, datetime):
            dt = value
        elif isinstance(value, str):
            try:
                dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
            except ValueError:
                dt = datetime.utcnow().replace(tzinfo=timezone.utc)
        else:
            dt = datetime.utcnow().replace(tzinfo=timezone.utc)

        if dt.tzinfo is None:
            dt = dt.replace(tzinfo=timezone.utc)
        return dt.astimezone(timezone.utc)

    def _current_file(self, ts: datetime) -> Path:
        suffix = ts.strftime("%Y-%m-%d") if self.rotation == "daily" else "default"
        return self.base_dir / f"vandal_events_{suffix}.jsonl"

    def _append_line(self, path: Path, line: str) -> None:
        with path.open("a", encoding="utf-8") as fp:
            fp.write(line + "\n")
