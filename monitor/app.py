#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Web backend for Wplace Vandalism Monitor (Web).
- Linux-first, no GUI deps
- FastAPI + WebSocket
- Periodically fetches tiles from wplace backend, crops area defined by REF_PIXEL
- Compares with reference image (PNG with alpha for mask) and streams base64 images + diff%
- Designed to work with the provided wplace_monitor.html which connects to ws://<host>:8000/ws

Environment variables (optional):
  PORT            (default: 8000)
  REF_IMAGE_PATH  (default: kiku.png)  # PNG with alpha channel as mask; same semantics as 最新版.py
  REF_PIXEL       (default: "1818,806,989,359")  # tile_x, tile_y, x_in_tile, y_in_tile
  TILE_SIZE       (default: 1000)
"""

import os
import io
import base64
import asyncio
import time
import logging
import struct # Added for binary packing
import random
import json
import math
from pathlib import Path
from typing import Tuple, List, Dict, Any, Optional, Set, Iterable
from concurrent.futures import ProcessPoolExecutor
from datetime import datetime, timezone
from collections import deque, defaultdict

import requests
import urllib.request
import urllib.error
from PIL import Image, ImageChops, ImageColor
import numpy as np

from fastapi import FastAPI, WebSocket, WebSocketDisconnect, Response, HTTPException, Request
from fastapi.responses import HTMLResponse, FileResponse
from fastapi.middleware.cors import CORSMiddleware
from starlette.websockets import WebSocketState

from analytics.event_logger import VandalEventLogger

# ------------------ Logging Config ------------------
logging.basicConfig(level=logging.DEBUG, format='%(asctime)s - %(levelname)s - %(message)s')

# ------------------ Config ------------------

PORT = int(os.getenv("PORT", "8000"))
REF_IMAGE_PATH = os.getenv("REF_IMAGE_PATH", "kiku.png")
REF_PIXEL_TEXT = os.getenv("REF_PIXEL", "1818,806,989,358")
# INTERVAL_MS is now hardcoded in the broadcast loop
TILE_SIZE = int(os.getenv("TILE_SIZE", "1000"))
TILES_BASE = os.getenv("TILES_BASE", "https://backend.wplace.live/files/s0/tiles")
WEIGHT_MASK_PATH = os.getenv("WEIGHT_MASK_PATH", "wplace_kiku_weight_mask.webp")
WEIGHT_DIFF_COLOR = "#7C3AED"  # purple
PIXEL_INFO_BASE_URL = "https://backend.wplace.live/s0/pixel"
PIXEL_INFO_RATE_LIMIT_INTERVAL = float(os.getenv("PIXEL_INFO_RATE_LIMIT_INTERVAL", "0.4"))  # default ~2.5 req/sec
PIXEL_INFO_BACKOFF_SECONDS = float(os.getenv("PIXEL_INFO_BACKOFF_SECONDS", "2.0"))
BASE_DIR = Path(__file__).resolve().parent
DATA_DIR = BASE_DIR / "data"
ACTIVITY_TRACKER_PATH = DATA_DIR / "user_activity.json"
ACTIVITY_STATE_FILE = DATA_DIR / "activity_state.json"
ANALYTICS_DIR = Path("/tmp/wplace_analytics")
# VANDAL_GRID_COLS = int(os.getenv("VANDAL_GRID_COLS", "6")) # グリッド制廃止
# VANDAL_GRID_ROWS = int(os.getenv("VANDAL_GRID_ROWS", "6")) # グリッド制廃止
# VANDAL_MAX_SAMPLES_PER_LOOP = max(1, VANDAL_GRID_COLS * VANDAL_GRID_ROWS) # グリッド制廃止
VANDAL_PIXEL_DEDUP_SECONDS = float(os.getenv("VANDAL_PIXEL_DEDUP_SECONDS", "60")) # これは残す (APIレートリミット用)
VANDAL_MAX_QUEUE_SIZE = int(os.getenv("VANDAL_MAX_QUEUE_SIZE", "5000"))
ADMIN_API_TOKEN = os.getenv("ADMIN_API_TOKEN")
VANDAL_RECENT_WINDOW_SECONDS = float(os.getenv("VANDAL_RECENT_WINDOW_SECONDS", "300"))
VANDAL_RECENT_PIXEL_THRESHOLD = int(os.getenv("VANDAL_RECENT_PIXEL_THRESHOLD", "5"))
VANDAL_PIXEL_MAX_AGE_SECONDS = float(os.getenv("VANDAL_PIXEL_MAX_AGE_SECONDS", "12"))
VANDAL_DETECTED_LOG_LIMIT = int(os.getenv("VANDAL_DETECTED_LOG_LIMIT", "400"))

PIXEL_DIFF_RGB_THRESHOLD = int(os.getenv("PIXEL_DIFF_RGB_THRESHOLD", "0")) # 厳密に差分を検知
PIXEL_DIFF_ALPHA_THRESHOLD = int(os.getenv("PIXEL_DIFF_ALPHA_THRESHOLD", "0")) # 厳密に差分を検知
logging.info(f"[VANDAL_CONFIG] VANDAL_RECENT_PIXEL_THRESHOLD loaded as: {VANDAL_RECENT_PIXEL_THRESHOLD}")

try:
    ANALYTICS_DIR.mkdir(parents=True, exist_ok=True)
except Exception as exc:
    logging.warning(f"[ANALYTICS] Failed to ensure analytics dir: {exc}")

try:
    VANDAL_EVENT_LOGGER = VandalEventLogger(ANALYTICS_DIR)
except Exception as exc:  # pragma: no cover
    logging.error(f"[ANALYTICS] Failed to initialize event logger: {exc}")
    VANDAL_EVENT_LOGGER = None

# ------------------ Utils (must be top-level for multiprocessing) ------------------

class PixelInfoRateLimitError(Exception):
    """Raised when the backend pixel info API returns HTTP 429."""
    pass

def safe_int_quad(text: str, default: Tuple[int, int, int, int]) -> Tuple[int, int, int, int]:
    try:
        parts = [int(v.strip()) for v in text.split(",")]
        if len(parts) != 4:
            return default
        return tuple(parts)
    except Exception:
        return default

DEFAULT_REF_PIXEL = (1818, 806, 989, 359)
REF_PIXEL = safe_int_quad(REF_PIXEL_TEXT, DEFAULT_REF_PIXEL)
REF_GLOBAL_ORIGIN_X = REF_PIXEL[0] * TILE_SIZE + REF_PIXEL[2]
REF_GLOBAL_ORIGIN_Y = REF_PIXEL[1] * TILE_SIZE + REF_PIXEL[3]

def get_image_from_url(url: str):
    try:
        resp = requests.get(url, timeout=8)
        resp.raise_for_status()
        return Image.open(io.BytesIO(resp.content))
    except requests.RequestException as e:
        # This log might not show up if run in a different process, but it's good practice
        logging.warning(f"画像取得失敗: {e}")
        return None

def compare_images(
    img1: Image.Image,
    img2: Image.Image,
    rgb_threshold: int = 45,
    alpha_threshold: int = 15
) -> Tuple[float, Image.Image, Tuple[int, int] | None, int, np.ndarray]:
    if img1.size != img2.size:
        w = min(img1.width, img2.width)
        h = min(img1.height, img2.height)
        img1 = img1.crop((0, 0, w, h))
        img2 = img2.crop((0, 0, w, h))

    img1 = img1.convert("RGBA")
    img2 = img2.convert("RGBA")

    arr1 = np.array(img1)
    arr2 = np.array(img2)

    alpha1 = arr1[:, :, 3].astype(np.int16)
    opaque_mask = alpha1 > 0
    opaque_pixels_count = int(np.count_nonzero(opaque_mask))
    if opaque_pixels_count == 0:
        empty_mask = np.zeros(opaque_mask.shape, dtype=bool)
        return 0.0, Image.new("RGBA", img1.size, (0, 0, 0, 0)), None, 0, empty_mask

    rgb1 = arr1[:, :, :3].astype(np.int16)
    rgb2 = arr2[:, :, :3].astype(np.int16)
    alpha2 = arr2[:, :, 3]

    diff_rgb = np.abs(rgb1 - rgb2)
    diff_sum_rgb = np.sum(diff_rgb, axis=2)
    diff_alpha = np.abs(alpha1 - alpha2)

    diff_rgb_mask = diff_sum_rgb > rgb_threshold
    diff_alpha_mask = diff_alpha > alpha_threshold

    diff_pixels_mask = np.logical_or(diff_rgb_mask, diff_alpha_mask)
    final_diff_mask = np.logical_and(opaque_mask, diff_pixels_mask)

    nz = int(np.count_nonzero(final_diff_mask))
    diff_pct = (nz / opaque_pixels_count) * 100.0 if opaque_pixels_count > 0 else 0.0

    changed_pixel_coord = None
    if nz > 0:
        y, x = np.argwhere(final_diff_mask)[0]
        changed_pixel_coord = (int(x), int(y))

    output_img = Image.new("RGBA", img1.size, (0, 0, 0, 0))
    green_layer = Image.new("RGBA", img1.size, (0, 255, 0, 255))
    mask_pil = Image.fromarray((final_diff_mask * 255).astype(np.uint8))
    output_img.paste(green_layer, (0, 0), mask_pil)

    return diff_pct, output_img, changed_pixel_coord, nz, final_diff_mask

def img_to_bytes(img: Image.Image) -> bytes:
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    return buf.getvalue()

def load_reference(path: str) -> Image.Image:
    p = Path(path)
    if not p.exists():
        raise FileNotFoundError(f"Reference image not found: {p.resolve()}")
    img = Image.open(p).convert("RGBA")
    return img


def hex_to_rgb(color_hex: str) -> Tuple[int, int, int]:
    """Convert #RRGGBB or RRGGBB into (r, g, b)."""
    color_hex = color_hex.strip()
    if color_hex.startswith("#"):
        color_hex = color_hex[1:]
    if len(color_hex) != 6:
        raise ValueError(f"Invalid hex color: {color_hex}")
    r = int(color_hex[0:2], 16)
    g = int(color_hex[2:4], 16)
    b = int(color_hex[4:6], 16)
    return r, g, b


def build_weight_config(ref_img: Image.Image) -> Optional[Dict[str, Any]]:
    """Create weight matrix giving chrysanthemum equal aggregate weight to background."""
    path = Path(WEIGHT_MASK_PATH)
    if not path.exists():
        logging.warning(f"[WEIGHT] Weight mask not found at {path}. Weighted diff disabled.")
        return None

    try:
        mask_img = Image.open(path).convert("RGBA")
    except Exception as exc:
        logging.error(f"[WEIGHT] Failed to load weight mask {path}: {exc}")
        return None

    if mask_img.size != ref_img.size:
        logging.error(f"[WEIGHT] Weight mask size {mask_img.size} does not match reference {ref_img.size}.")
        return None

    ref_alpha = np.array(ref_img)[:, :, 3] > 0
    mask_alpha = np.array(mask_img)[:, :, 3] > 0

    chrysanthemum_mask = np.logical_and(ref_alpha, mask_alpha)
    background_mask = np.logical_and(ref_alpha, ~chrysanthemum_mask)

    chrys_count = int(np.count_nonzero(chrysanthemum_mask))
    background_count = int(np.count_nonzero(background_mask))

    if chrys_count == 0 or background_count == 0:
        logging.error(f"[WEIGHT] Invalid mask counts (chrysanthemum={chrys_count}, background={background_count}).")
        return None

    chrys_weight = background_count / chrys_count

    weights = np.zeros(ref_alpha.shape, dtype=np.float32)
    weights[chrysanthemum_mask] = chrys_weight
    weights[background_mask] = 1.0

    total_weight = float(weights.sum())

    logging.info(
        f"[WEIGHT] Enabled weighted diff with chrysanthemum weight {chrys_weight:.3f} "
        f"({chrys_count=} background_count={background_count} total_weight={total_weight:.1f})."
    )

    return {
        "matrix": weights,
        "total_weight": total_weight,
        "color": WEIGHT_DIFF_COLOR,
        "chrysanthemum_mask": chrysanthemum_mask,
        "background_mask": background_mask,
        "chrysanthemum_pixels": chrys_count,
        "background_pixels": background_count,
    }


WEIGHT_CONFIG: Optional[Dict[str, Any]] = None
REF_ALPHA_MASK: Optional[np.ndarray] = None
PENDING_ACTIVITY_TASKS: Set[asyncio.Task] = set()


class PixelActivityTracker:
    """Track vandal user metadata from pixel API responses."""

    def __init__(self, path: Path):
        self.user_data_path = path # vandal_users.json
        self.pixel_state_path = ACTIVITY_STATE_FILE # vandalized_pixels.json
        self.rate_limit_interval = PIXEL_INFO_RATE_LIMIT_INTERVAL
        self.lock = asyncio.Lock()
        self.rate_lock = asyncio.Lock()
        self.last_request_ts = 0.0
        self.data: Dict[str, Dict[str, Any]] = {} # vandal_users.jsonの内容
        self.vandalized_pixels: Set[Tuple[int, int]] = set() # 現在荒らされているピクセルのグローバル座標
        self.pixel_to_painter: Dict[Tuple[int, int], str] = {} # ピクセル -> 荒らしたユーザーID
        self.pending_queue: asyncio.Queue[Tuple[int, int]] = asyncio.Queue()
        self.pending_coords: Set[Tuple[int, int]] = set()
        self.recent_pixel_ts: Dict[Tuple[int, int], float] = {}
        self.pixel_detected_at: Dict[Tuple[int, int], float] = {}
        self.last_diff_snapshot: Set[Tuple[int, int]] = set()
        self.worker_task: Optional[asyncio.Task] = None
        self._last_recent_prune = 0.0
        self.painter_recent_pixels: Dict[str, deque] = defaultdict(deque)
        self.event_logger = VANDAL_EVENT_LOGGER
        if self.user_data_path.parent and not self.user_data_path.parent.exists():
            self.user_data_path.parent.mkdir(parents=True, exist_ok=True)
        if self.pixel_state_path.parent and not self.pixel_state_path.parent.exists():
            self.pixel_state_path.parent.mkdir(parents=True, exist_ok=True)
        self._load_existing()

    def _load_existing(self):
        # user_activity.json の読み込み
        if self.user_data_path.exists():
            try:
                with open(self.user_data_path, "r", encoding="utf-8") as f:
                    payload = json.load(f)
                    if isinstance(payload, dict):
                        # データ移行ロジック
                        for key, user_data in payload.items():
                            if 'pixel_count' in user_data:
                                user_data['vandal_count'] = user_data.pop('pixel_count', 0)
                            if 'daily_pixel_counts' in user_data:
                                user_data['daily_vandal_counts'] = user_data.pop('daily_pixel_counts', {})
                            user_data.setdefault('restored_count', 0)
                            user_data.setdefault('daily_restored_counts', {})
                        self.data = {str(k): v for k, v in payload.items()}
                        logging.info(f"[ACTIVITY] Loaded {len(self.data)} user entries from {self.user_data_path}")
            except Exception as exc:
                logging.warning(f"[ACTIVITY] Failed to load existing user data: {exc}")

        # activity_state.json の読み込み
        if self.pixel_state_path.exists():
            try:
                with open(self.pixel_state_path, "r", encoding="utf-8") as f:
                    payload = json.load(f)
                    if isinstance(payload, dict):
                        # JSONから読み込んだリストをSetに変換
                        self.vandalized_pixels = set(tuple(p) for p in payload.get("vandalized_pixels", []))
                        self.pixel_to_painter = {tuple(map(int, (x.strip() for x in p.strip('()').split(',')))): painter_id for p, painter_id in payload.get("pixel_to_painter", {}).items() if p}
                        logging.info(f"[ACTIVITY] Loaded {len(self.vandalized_pixels)} vandalized pixels from {self.pixel_state_path}")
            except Exception as exc:
                logging.warning(f"[ACTIVITY] Failed to load existing pixel state: {exc}")

    def ensure_worker(self):
        """Start the background worker that drains the pending pixel queue."""
        if self.worker_task and not self.worker_task.done():
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return
        self.worker_task = loop.create_task(self._pixel_worker())

        def _on_done(task: asyncio.Task):
            try:
                task.result()
            except Exception as exc:  # pragma: no cover
                logging.error(f"[VANDAL] Pixel worker crashed: {exc}", exc_info=True)

        self.worker_task.add_done_callback(_on_done)

    def _prune_recent_cache(self, now: float):
        """Remove stale deduplication entries."""
        if now - self._last_recent_prune < VANDAL_PIXEL_DEDUP_SECONDS:
            return
        cutoff = now - (VANDAL_PIXEL_DEDUP_SECONDS * 2)
        stale_keys = [coord for coord, ts in self.recent_pixel_ts.items() if ts < cutoff]
        for coord in stale_keys:
            self.recent_pixel_ts.pop(coord, None)
        self._last_recent_prune = now

    def _painter_threshold_state(self, painter_id: str) -> Tuple[bool, int, int]:
        now = time.monotonic()
        dq = self.painter_recent_pixels[painter_id]
        cutoff = now - VANDAL_RECENT_WINDOW_SECONDS
        while dq and dq[0] < cutoff:
            dq.popleft()

        current_count = len(dq)
        threshold = VANDAL_RECENT_PIXEL_THRESHOLD
        # Check if the incoming pixel meets or exceeds the threshold
        is_exceeded = (current_count + 1) >= threshold
        dq.append(now)
        window_count = len(dq)

        logging.info(
            f"[VANDAL_DEBUG] Checking painter {painter_id}: "
            f"current_count={current_count}, threshold={threshold}, "
            f"is_exceeded={is_exceeded}"
        )

        return is_exceeded, window_count, threshold

    async def process_diff_pixels(self, global_diff_pixels: List[Tuple[int, int]], current_ref_img_mask: np.ndarray, base_global_x: int, base_global_y):
        """差分ピクセルを荒らし・修復に分類し、バックグラウンドワーカー用キューに投入する。"""
        self.ensure_worker()
        diff_set = {(int(px), int(py)) for px, py in global_diff_pixels}

        enqueued_vandals: List[Tuple[int, int]] = []
        enqueued_restores: List[Tuple[int, int]] = []
        snapshot_coords: Optional[Set[Tuple[int, int]]] = None

        async with self.lock:
            # 以前は荒らしだったが、現在は差分がないピクセル = 修復されたピクセル
            logging.debug(f"[DEBUG] Before restoration check - vandalized_pixels: {self.vandalized_pixels}, diff_set: {diff_set}")
            restored_pixels = self.vandalized_pixels - diff_set
            if restored_pixels:
                logging.debug(f"[DEBUG] Identified restored_pixels: {restored_pixels}")
                for pixel_coord in restored_pixels:
                    self.vandalized_pixels.discard(pixel_coord)
                    self.pixel_to_painter.pop(pixel_coord, None)
                    self.pixel_detected_at.pop(pixel_coord, None)
                    if pixel_coord not in self.pending_coords:
                        self.pending_coords.add(pixel_coord)
                    enqueued_restores.append(pixel_coord)
                    logging.debug(f"[ACTIVITY] Pixel {pixel_coord} restored. Queuing for restore credit.")
                await self._save_locked()
            else:
                logging.debug("[DEBUG] No restored pixels identified.")

            # 前回差分から消えたピクセルも修復扱いとし、即時クレジットする
            additional_restored = (self.last_diff_snapshot - diff_set) - set(restored_pixels)
            if additional_restored:
                logging.debug(f"[DEBUG] Additional restored pixels detected via snapshot diff: {additional_restored}")
                for pixel_coord in additional_restored:
                    if pixel_coord in self.pending_coords:
                        continue
                    rel_x = pixel_coord[0] - base_global_x
                    rel_y = pixel_coord[1] - base_global_y
                    if not (0 <= rel_y < current_ref_img_mask.shape[0] and 0 <= rel_x < current_ref_img_mask.shape[1]):
                        continue
                    if not current_ref_img_mask[rel_y, rel_x]:
                        continue
                    if len(self.pending_coords) >= VANDAL_MAX_QUEUE_SIZE:
                        logging.warning(f"[ACTIVITY] Pending queue full ({VANDAL_MAX_QUEUE_SIZE}). Remaining restores will be retried later.")
                        break
                    self.pending_coords.add(pixel_coord)
                    enqueued_restores.append(pixel_coord)
                    logging.debug(f"[ACTIVITY] Snapshot restore queued for {pixel_coord}.")

            # 新たに発生した差分ピクセル = 荒らし候補
            candidate_pixels = diff_set - self.vandalized_pixels - self.pending_coords
            if not candidate_pixels:
                logging.info("[ACTIVITY] No new vandalized pixels to queue.")
                if diff_set:
                    snapshot_coords = set(diff_set)
            else:
                now = time.monotonic()
                self._prune_recent_cache(now)

                for pixel_coord in candidate_pixels:
                    rel_x = pixel_coord[0] - base_global_x
                    rel_y = pixel_coord[1] - base_global_y
                    if not (0 <= rel_y < current_ref_img_mask.shape[0] and 0 <= rel_x < current_ref_img_mask.shape[1]):
                        logging.debug(f"[ACTIVITY] Pixel {pixel_coord} is outside monitor area, ignoring.")
                        continue
                    if not current_ref_img_mask[rel_y, rel_x]:
                        logging.debug(f"[ACTIVITY] Pixel {pixel_coord} is outside reference mask, ignoring.")
                        continue
                    if len(self.pending_coords) >= VANDAL_MAX_QUEUE_SIZE:
                        logging.warning(f"[ACTIVITY] Pending queue full ({VANDAL_MAX_QUEUE_SIZE}). Remaining pixels will be retried later.")
                        break
                    last_seen = self.recent_pixel_ts.get(pixel_coord)
                    if last_seen and (now - last_seen) < VANDAL_PIXEL_DEDUP_SECONDS:
                        continue
                    self.recent_pixel_ts[pixel_coord] = now
                    self.pending_coords.add(pixel_coord)
                    self.pixel_detected_at[pixel_coord] = now
                    enqueued_vandals.append(pixel_coord)

            # 次回比較用に差分スナップショットを更新
            self.last_diff_snapshot = set(diff_set)

        if snapshot_coords:
            await self._log_detected_snapshot(snapshot_coords)

        # キューに追加
        for pixel_coord in enqueued_vandals:
            self.pending_queue.put_nowait(('vandal', pixel_coord))

        for pixel_coord in enqueued_restores:
            self.pending_queue.put_nowait(('restore', pixel_coord))

        if enqueued_vandals or enqueued_restores:
            logging.info(f"[ACTIVITY] Queued {len(enqueued_vandals)} vandal(s) and {len(enqueued_restores)} restore(s) for lookup (pending={self.pending_queue.qsize()}).")

    async def _log_detected_snapshot(self, pixel_coords: Iterable[Tuple[int, int]]):
        if not self.event_logger:
            return

        for idx, coord in enumerate(pixel_coords):
            if idx >= VANDAL_DETECTED_LOG_LIMIT:
                break

            timestamp = datetime.utcnow().isoformat() + "Z"
            payload = {
                "painter_id": "detected",
                "painter_name": "",
                "alliance": "",
                "pixel_x": int(coord[0]),
                "pixel_y": int(coord[1]),
                "window_seconds": VANDAL_RECENT_WINDOW_SECONDS,
                "window_count": 0,
                "threshold": VANDAL_RECENT_PIXEL_THRESHOLD,
                "is_vandalized": True,
                "total_pixels_recorded": 0,
                "todays_pixels": 0,
                "timestamp": timestamp,
                "detected_only": True,
            }
            try:
                await self.event_logger.log(payload)
            except Exception as exc:  # pragma: no cover
                logging.debug(f"[ANALYTICS] Failed to log snapshot event: {exc}")

    async def _fetch_and_update_activity_info(self, pixel_coord: Tuple[int, int], activity_type: str):
        """単一ピクセルの情報を取得し、ユーザーデータを更新する"""
        global_px, global_py = pixel_coord
        tile_x = global_px // TILE_SIZE
        tile_y = global_py // TILE_SIZE
        x_in_tile = global_px % TILE_SIZE
        y_in_tile = global_py % TILE_SIZE

        url = f"{PIXEL_INFO_BASE_URL}/{tile_x}/{tile_y}?x={x_in_tile}&y={y_in_tile}"

        if activity_type == 'vandal':
            detected_ts = self.pixel_detected_at.get(pixel_coord)
            if detected_ts is not None:
                age = time.monotonic() - detected_ts
                if age > VANDAL_PIXEL_MAX_AGE_SECONDS:
                    logging.info(
                        f"[VANDAL] Pixel {pixel_coord} stale ({age:.2f}s), skipping to avoid false positive."
                    )
                    return False
        try:
            payload = await self._rate_limited_request(url)
        except PixelInfoRateLimitError:
            logging.warning(f"[VANDAL] Pixel info rate limit hit while processing {pixel_coord}.")
            raise

        if not payload:
            logging.warning(f"[VANDAL] Failed to fetch pixel info for {pixel_coord}. Skipping update.")
            return False
        painter = payload.get("paintedBy")
        if not isinstance(painter, dict):
            logging.warning(f"[VANDAL] No painter info for {pixel_coord}. Skipping update.")
            return False
        painter_id = painter.get("id")
        if painter_id is None:
            logging.warning(f"[VANDAL] Painter ID is None for {pixel_coord}. Skipping update.")
            return False

        painter_id_str = str(painter_id)

        # ユーザーデータと状態を常に更新（ピクセル数をカウント）
        async with self.lock:
            count, daily_count = await self._update_user_data(
                painter_id_str, painter, pixel_coord, activity_type
            )

        # 荒らし活動の場合のみ、閾値チェックと永続化を行う
        if activity_type == 'vandal':
            # 閾値を超えたかどうかをチェック
            is_vandal, window_count, threshold = self._painter_threshold_state(painter_id_str)

            await self._log_pixel_event(
                painter_id=painter_id_str,
                painter=painter,
                pixel_coord=pixel_coord,
                window_count=window_count,
                threshold=threshold,
                is_vandal=is_vandal,
                total_pixels=count,
                todays_pixels=daily_count,
                activity_type=activity_type,
            )

            if not is_vandal:
                logging.info(
                    f"[ACTIVITY] Painter {painter_id_str} below threshold "
                    f"({VANDAL_RECENT_PIXEL_THRESHOLD} px / {VANDAL_RECENT_WINDOW_SECONDS}s). Not marking as vandal for pixel {pixel_coord}."
                )
                return False # 荒らしとしてマークしない

            still_diff = await self._pixel_still_differs(pixel_coord)
            if still_diff is False:
                logging.info(f"[ACTIVITY] Pixel {pixel_coord} already restored before confirmation. Skipping.")
                return False
            if still_diff is None:
                logging.info(f"[ACTIVITY] Could not confirm state of pixel {pixel_coord}. Skipping to avoid false positive.")
                return False

            # 閾値を超えた場合のみ、荒らしピクセルとして記録
            async with self.lock:
                logging.debug(f"[DEBUG] Before adding: vandalized_pixels={self.vandalized_pixels}, pixel_coord={pixel_coord}")
                self.vandalized_pixels.add(pixel_coord)
                self.pixel_to_painter[pixel_coord] = painter_id_str
                logging.debug(f"[DEBUG] After adding: vandalized_pixels={self.vandalized_pixels}")
                await self._save_locked()

            logging.info(f"[ACTIVITY] Recorded pixel {pixel_coord} by painter {painter_id} as vandalized.")
            return True

        elif activity_type == 'restore':
            # 修復活動のログを記録
            await self._log_pixel_event(
                painter_id=painter_id_str,
                painter=painter,
                pixel_coord=pixel_coord,
                window_count=0, # 修復には閾値がないため0
                threshold=0,    # 同上
                is_vandal=False,
                total_pixels=count,
                todays_pixels=daily_count,
                activity_type=activity_type,
            )
            # 修復カウントを反映した最新状態を保存する
            async with self.lock:
                await self._save_locked()
            logging.info(f"[ACTIVITY] Recorded pixel {pixel_coord} by painter {painter_id} as restored.")
            return True

        return False

    async def _log_pixel_event(
        self,
        painter_id: str,
        painter: Dict[str, Any],
        pixel_coord: Tuple[int, int],
        window_count: int,
        threshold: int,
        is_vandal: bool,
        total_pixels: int,
        todays_pixels: int,
        activity_type: str,
    ) -> None:
        """Persist per-pixel events for downstream analytics."""
        if not self.event_logger:
            return

        event_payload = {
            "painter_id": painter_id,
            "painter_name": painter.get("name") or "",
            "alliance": painter.get("allianceName") or "",
            "pixel_x": int(pixel_coord[0]),
            "pixel_y": int(pixel_coord[1]),
            "window_seconds": VANDAL_RECENT_WINDOW_SECONDS,
            "window_count": window_count,
            "threshold": threshold,
            "is_vandalized": activity_type == 'vandal' and is_vandal,
            "is_restored": activity_type == 'restore',
            "total_vandal_pixels": total_pixels if activity_type == 'vandal' else self.data.get(painter_id, {}).get('vandal_count', 0),
            "total_restored_pixels": total_pixels if activity_type == 'restore' else self.data.get(painter_id, {}).get('restored_count', 0),
            "todays_vandal_pixels": todays_pixels if activity_type == 'vandal' else self.data.get(painter_id, {}).get('daily_vandal_counts', {}).get(datetime.utcnow().strftime("%Y-%m-%d"), 0),
            "todays_restored_pixels": todays_pixels if activity_type == 'restore' else self.data.get(painter_id, {}).get('daily_restored_counts', {}).get(datetime.utcnow().strftime("%Y-%m-%d"), 0),
            "timestamp": datetime.utcnow().replace(tzinfo=timezone.utc).isoformat().replace("+00:00", "Z"),
        }

        try:
            await self.event_logger.log(event_payload)
        except Exception as exc:  # pragma: no cover
            logging.debug(f"[ANALYTICS] Failed to log event: {exc}")

    async def _pixel_still_differs(self, pixel_coord: Tuple[int, int]) -> Optional[bool]:
        """最新タイルを確認し、まだ差分があるかを検証する。"""

        def _fetch_pixel_value() -> Optional[Tuple[int, int, int, int]]:
            global_px, global_py = pixel_coord
            tile_x = global_px // TILE_SIZE
            tile_y = global_py // TILE_SIZE
            x_in_tile = global_px % TILE_SIZE
            y_in_tile = global_py % TILE_SIZE
            url = f"{TILES_BASE}/{tile_x}/{tile_y}.png?t={random.randint(1000, 9999)}"
            img = get_image_from_url(url)
            if img is None:
                return None
            img = img.convert("RGBA")
            if not (0 <= x_in_tile < img.width and 0 <= y_in_tile < img.height):
                return None
            return img.getpixel((x_in_tile, y_in_tile))

        live_pixel = await asyncio.to_thread(_fetch_pixel_value)
        if live_pixel is None:
            return None

        ref_pixel = self._get_reference_pixel(pixel_coord)
        if ref_pixel is None:
            return None

        rgb_diff = sum(abs(int(a) - int(b)) for a, b in zip(live_pixel[:3], ref_pixel[:3]))
        alpha_diff = abs(int(live_pixel[3]) - int(ref_pixel[3]))
        return rgb_diff > PIXEL_DIFF_RGB_THRESHOLD or alpha_diff > PIXEL_DIFF_ALPHA_THRESHOLD

    def _get_reference_pixel(self, pixel_coord: Tuple[int, int]) -> Optional[Tuple[int, int, int, int]]:
        if REF_IMG is None:
            return None
        local_x = pixel_coord[0] - REF_GLOBAL_ORIGIN_X
        local_y = pixel_coord[1] - REF_GLOBAL_ORIGIN_Y
        if not (0 <= local_x < REF_IMG.width and 0 <= local_y < REF_IMG.height):
            return None
        return REF_IMG.getpixel((local_x, local_y))


    def _decrement_daily_counter(self, daily_counts: Dict[str, int]):
        """Reduce one count from the most recent day that still has remaining entries."""
        if not daily_counts:
            return
        for day in sorted(daily_counts.keys(), reverse=True):
            count = daily_counts.get(day, 0)
            if count > 0:
                new_count = count - 1
                if new_count > 0:
                    daily_counts[day] = new_count
                else:
                    daily_counts.pop(day, None)
                break

    def _rebalance_legacy_counts(self, painter_id: str, user_data: Dict[str, Any]):
        """
        旧ロジックで加算済みの修復ピクセルが残っている場合、
        荒らしカウントを優先して相殺し、余剰分のみ修復として残す。
        """
        if user_data.get("_offset_legacy_applied_v1"):
            return

        vandal = max(0, user_data.get("vandal_count", 0))
        restored = max(0, user_data.get("restored_count", 0))
        if vandal and restored:
            offset = min(vandal, restored)
            if offset:
                user_data["vandal_count"] = vandal - offset
                user_data["restored_count"] = restored - offset
                for _ in range(offset):
                    self._decrement_daily_counter(user_data.get("daily_vandal_counts", {}))
                    self._decrement_daily_counter(user_data.get("daily_restored_counts", {}))
                logging.info(
                    f"[ACTIVITY] Applied legacy offset ({offset}) for painter {painter_id}. "
                    f"Totals -> vandal:{user_data['vandal_count']} restored:{user_data['restored_count']}"
                )

        user_data["_offset_legacy_applied_v1"] = True

    async def _update_user_data(self, painter_id: str, painter_info: Dict[str, Any], pixel_coord: Tuple[int, int], activity_type: str):
        """ユーザーの活動（荒らし or 修復）ピクセル数を更新する"""
        timestamp = datetime.utcnow().isoformat() + "Z"
        current_date_str = datetime.utcnow().strftime("%Y-%m-%d")

        key = painter_id
        if key not in self.data:
            # 新規ユーザー
            self.data[key] = {
                "id": painter_id,
                "name": painter_info.get("name") or "",
                "allianceName": painter_info.get("allianceName") or "",
                "last_seen": timestamp,
                "vandal_count": 0,
                "restored_count": 0,
                "daily_vandal_counts": {},
                "daily_restored_counts": {},
                "last_daily_reset": current_date_str,
            }
            logging.info(f"[ACTIVITY] Initialized new painter {painter_id} ({painter_info.get('name')}).")

        user_data = self.data[key]
        user_data['last_seen'] = timestamp
        if painter_info.get('name'): user_data['name'] = painter_info['name']
        if painter_info.get('allianceName'): user_data['allianceName'] = painter_info['allianceName']

        # 古いデータ構造からの移行
        if 'pixel_count' in user_data:
            user_data['vandal_count'] = user_data.pop('pixel_count', 0)
        if 'daily_pixel_counts' in user_data:
            user_data['daily_vandal_counts'] = user_data.pop('daily_pixel_counts', {})

        # カウントの初期化
        user_data.setdefault('vandal_count', 0)
        user_data.setdefault('restored_count', 0)
        user_data.setdefault('daily_vandal_counts', {})
        user_data.setdefault('daily_restored_counts', {})
        user_data.setdefault('last_daily_reset', current_date_str)

        # 旧データの整合性を一度だけ補正
        self._rebalance_legacy_counts(painter_id, user_data)

        # 日付が変わっていたらデイリーカウントをリセット
        if user_data['last_daily_reset'] != current_date_str:
            user_data['daily_vandal_counts'] = {}
            user_data['daily_restored_counts'] = {}
            user_data['last_daily_reset'] = current_date_str

        # 活動タイプに応じてカウントをインクリメント
        if activity_type == 'vandal':
            user_data['vandal_count'] += 1
            user_data['daily_vandal_counts'][current_date_str] = user_data['daily_vandal_counts'].get(current_date_str, 0) + 1
            user_data['last_pixel'] = {
                "x": int(pixel_coord[0]),
                "y": int(pixel_coord[1]),
            }
            logging.info(f"[ACTIVITY] Vandalism by {painter_id}. Total: {user_data['vandal_count']}, Daily: {user_data['daily_vandal_counts'][current_date_str]}")
            return user_data['vandal_count'], user_data['daily_vandal_counts'][current_date_str]

        elif activity_type == 'restore':
            if user_data['vandal_count'] > 0:
                user_data['vandal_count'] -= 1
                self._decrement_daily_counter(user_data['daily_vandal_counts'])
                logging.info(
                    f"[ACTIVITY] Restoration by {painter_id} offset a previous vandalism. "
                    f"Remaining vandal count: {user_data['vandal_count']}"
                )
                logging.debug(
                    f"[DEBUG] Offset vandal count for {painter_id}: total={user_data['vandal_count']}"
                )
                daily_restored = user_data['daily_restored_counts'].get(current_date_str, 0)
                return user_data['restored_count'], daily_restored

            user_data['restored_count'] += 1
            user_data['daily_restored_counts'][current_date_str] = user_data['daily_restored_counts'].get(current_date_str, 0) + 1
            logging.info(f"[ACTIVITY] Restoration by {painter_id}. Total: {user_data['restored_count']}, Daily: {user_data['daily_restored_counts'][current_date_str]}")
            logging.debug(f"[DEBUG] Restored count for {painter_id}: total={user_data['restored_count']}, daily={user_data['daily_restored_counts'][current_date_str]}")
            return user_data['restored_count'], user_data['daily_restored_counts'][current_date_str]

        return 0, 0

    def _write_file(self, path: Path, data: Dict[str, Any]):
        tmp_path = path.with_suffix(".tmp")
        with open(tmp_path, "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)
        tmp_path.replace(path)

    async def _save_locked(self):
        """Persist vandal user data and pixel state (caller must hold self.lock)."""
        user_data_snapshot = dict(self.data)
        await asyncio.to_thread(self._write_file, self.user_data_path, user_data_snapshot)

        pixel_state_snapshot = {
            "vandalized_pixels": [[int(x), int(y)] for (x, y) in self.vandalized_pixels],
            "pixel_to_painter": {str((int(x), int(y))): v for (x, y), v in self.pixel_to_painter.items()},
        }
        await asyncio.to_thread(self._write_file, self.pixel_state_path, pixel_state_snapshot)

    async def save(self):
        """Persist state while managing locking internally."""
        async with self.lock:
            await self._save_locked()

    async def delete_painter(self, painter_id: str) -> Dict[str, Any]:
        """Remove a painter and all associated pixel records."""
        painter_id = str(painter_id)
        removed_pixels: List[Tuple[int, int]] = []
        async with self.lock:
            removed_user = painter_id in self.data
            if removed_user:
                self.data.pop(painter_id, None)
                self.painter_recent_pixels.pop(painter_id, None)

            for coord, pid in list(self.pixel_to_painter.items()):
                if pid == painter_id:
                    removed_pixels.append(coord)
                    self.pixel_to_painter.pop(coord, None)
                    self.vandalized_pixels.discard(coord)

            for coord in removed_pixels:
                self.pending_coords.discard(coord)
                self.recent_pixel_ts.pop(coord, None)

            await self._save_locked()

        return {
            "removed_user": removed_user,
            "removed_pixels": len(removed_pixels),
        }

    async def _rate_limited_request(self, url: str) -> Optional[Dict[str, Any]]:
        async with self.rate_lock:
            now = time.monotonic()
            wait = self.rate_limit_interval - (now - self.last_request_ts)
            if wait > 0:
                await asyncio.sleep(wait)
            self.last_request_ts = time.monotonic()
        return await asyncio.to_thread(self._sync_fetch_json, url)

    async def _pixel_worker(self):
        logging.info("[ACTIVITY] Pixel info worker started.")
        while True:
            activity_type, pixel_coord = await self.pending_queue.get()
            try:
                await self._process_pending_pixel(activity_type, pixel_coord)
            finally:
                async with self.lock:
                    self.pending_coords.discard(pixel_coord)
                    self.pixel_detected_at.pop(pixel_coord, None)
                self.pending_queue.task_done()

    async def _process_pending_pixel(self, activity_type: str, pixel_coord: Tuple[int, int]):
        attempt = 0
        while True:
            try:
                await self._fetch_and_update_activity_info(pixel_coord, activity_type)
                return
            except PixelInfoRateLimitError:
                attempt += 1
                backoff = PIXEL_INFO_BACKOFF_SECONDS * max(1, attempt)
                logging.warning(f"[ACTIVITY] Rate limit hit while processing {pixel_coord}. Retrying in {backoff:.1f}s (attempt {attempt}).")
                await asyncio.sleep(backoff)
            except Exception as exc:
                logging.warning(f"[ACTIVITY] Unexpected error while processing pixel {pixel_coord}: {exc}", exc_info=True)
                return

    @staticmethod
    def _sync_fetch_json(url: str) -> Optional[Dict[str, Any]]:
        headers = {
            "User-Agent": "python-urllib/3 fetch_pixel_json",
            "Accept": "application/json, */*;q=0.9",
        }
        req = urllib.request.Request(url, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=5) as resp:
                data = resp.read()
                text = data.decode("utf-8", errors="replace")
                return json.loads(text)
        except urllib.error.HTTPError as exc:
            if exc.code == 429:
                raise PixelInfoRateLimitError from exc
            logging.warning(f"[VANDAL] Failed to fetch pixel info: {exc}")
            return None
        except (urllib.error.URLError, json.JSONDecodeError) as exc:
            logging.warning(f"[VANDAL] Failed to fetch pixel info: {exc}")
            return None


ACTIVITY_TRACKER = PixelActivityTracker(ACTIVITY_TRACKER_PATH)

async def handle_activity_payload(payload: Optional[Dict[str, Any]]):
    """Forward diff pixels detected in worker process to the vandal tracker."""
    if not payload:
        return
    if ACTIVITY_TRACKER is None:
        return
    if REF_ALPHA_MASK is None:
        logging.warning("[VANDAL] Reference alpha mask is unavailable; skipping update.")
        return

    global_diff_pixels = payload.get("global_diff_pixels") or []
    base_global_x = payload.get("base_global_x")
    base_global_y = payload.get("base_global_y")

    if base_global_x is None or base_global_y is None:
        base_global_x = REF_PIXEL[0] * TILE_SIZE + REF_PIXEL[2]
        base_global_y = REF_PIXEL[1] * TILE_SIZE + REF_PIXEL[3]

    try:
        await ACTIVITY_TRACKER.process_diff_pixels(
            global_diff_pixels,
            REF_ALPHA_MASK,
            base_global_x,
            base_global_y,
        )
    except Exception:
        logging.error("[VANDAL] Failed to process diff pixels", exc_info=True)

def schedule_activity_payload_processing(payload: Optional[Dict[str, Any]]):
    """Dispatch vandal payload processing without blocking the main loop."""
    if not payload:
        return
    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        logging.warning("[VANDAL] Event loop not running; skipping payload processing.")
        return

    task = loop.create_task(handle_activity_payload(payload))
    PENDING_ACTIVITY_TASKS.add(task)

    def _cleanup(fut: asyncio.Task):
        PENDING_ACTIVITY_TASKS.discard(fut)
        if fut.cancelled():
            return
        exc = fut.exception()
        if exc:
            logging.error("[VANDAL] Background payload task failed: %s", exc, exc_info=True)

    task.add_done_callback(_cleanup)

# --- Image Message Protocol ---
IMAGE_TYPE_REF = 1
IMAGE_TYPE_LIVE = 2
IMAGE_TYPE_DIFF = 3
IMAGE_TYPE_RAW_TILE = 4
IMAGE_TYPE_MINIMAP = 5
IMAGE_TYPE_MAP = {"ref": IMAGE_TYPE_REF, "live": IMAGE_TYPE_LIVE, "diff": IMAGE_TYPE_DIFF, "raw_tile": IMAGE_TYPE_RAW_TILE, "minimap": IMAGE_TYPE_MINIMAP}

def create_image_message(image_type: str, image_bytes: bytes) -> bytes:
    type_id = IMAGE_TYPE_MAP.get(image_type)
    if type_id is None:
        raise ValueError(f"Unknown image type: {image_type}")
    header = struct.pack('<BI', type_id, len(image_bytes))
    return header + image_bytes

# --- CPU-Bound Processing (for ProcessPoolExecutor) ---

def stitch_and_crop_tiles(tiles: Dict[Tuple[int, int], Image.Image],
                          start_tx: int, start_ty: int, end_tx: int, end_ty: int,
                          global_x: int, global_y: int, width: int, height: int, tile_size: int) -> Image.Image | None:
    """Synchronous, CPU-bound tile stitching and cropping."""
    try:
        comb_w = (end_tx - start_tx + 1) * tile_size
        comb_h = (end_ty - start_ty + 1) * tile_size
        combined = Image.new("RGBA", (comb_w, comb_h))
        for (tx, ty), im in tiles.items():
            px = (tx - start_tx) * tile_size
            py = (ty - start_ty) * tile_size
            combined.paste(im, (px, py))

        cx1 = global_x - start_tx * tile_size
        cy1 = global_y - start_ty * tile_size
        cx2 = cx1 + width
        cy2 = cy1 + height
        return combined.crop((cx1, cy1, cx2, cy2))
    except Exception as e:
        logging.error(f"タイル結合・クロップ中にエラー: {e}", exc_info=True)
        return None

def process_live_image(
    ref_img: Image.Image,
    live_img: Image.Image,
    rgb_threshold: int = 45,
    alpha_threshold: int = 15,
    weight_config: Optional[Dict[str, Any]] = None
) -> Dict[str, Any] | None:
    """Synchronous, CPU-bound image comparison that runs in a separate process."""
    try:
        # Define aligned_live_img FIRST
        aligned_live_img = Image.new("RGBA", ref_img.size, (0, 0, 0, 0))
        aligned_live_img.paste(live_img, (0, 0))

        # THEN use it in compare_images
        diff_pct, diff_img, changed_pixel_coord, diff_pixels, diff_mask = compare_images(
            ref_img, aligned_live_img, rgb_threshold, alpha_threshold
        )
        # vandal_samples = sample_diff_coordinates(diff_mask, VANDAL_GRID_COLS, VANDAL_GRID_ROWS) # グリッドサンプリング廃止

        # 荒らされたピクセルをすべて取得し、グローバル座標に変換
        global_diff_pixels = []
        if diff_pixels > 0:
            ref_alpha_mask = np.array(ref_img)[:, :, 3] > 0 # 参照画像の不透明マスク
            # diff_mask は ref_img のサイズに合わせられているはず
            relative_diff_coords = np.argwhere(diff_mask)
            for ry, rx in relative_diff_coords:
                # 参照画像の有効な領域内のピクセルのみを対象とする
                if ref_alpha_mask[ry, rx]:
                    global_px = REF_PIXEL[0] * TILE_SIZE + REF_PIXEL[2] + rx
                    global_py = REF_PIXEL[1] * TILE_SIZE + REF_PIXEL[3] + ry
                    global_diff_pixels.append((global_px, global_py))

        # ACTIVITY_TRACKER 処理用に監視領域の原点も保持
        base_global_x = REF_PIXEL[0] * TILE_SIZE + REF_PIXEL[2]
        base_global_y = REF_PIXEL[1] * TILE_SIZE + REF_PIXEL[3]
        activity_payload = {
            "global_diff_pixels": global_diff_pixels,
            "base_global_x": base_global_x,
            "base_global_y": base_global_y,
        }

        metadata_payload = {
            "type": "metadata",
            "diff_percentage": round(float(diff_pct), 2),
            "diff_pixels": diff_pixels,
        }

        if weight_config:
            weights = weight_config.get("matrix")
            total_weight = weight_config.get("total_weight", 0.0)
            chrys_mask = weight_config.get("chrysanthemum_mask")
            background_mask = weight_config.get("background_mask")
            chrys_total_pixels = int(weight_config.get("chrysanthemum_pixels", 0))
            background_total_pixels = int(weight_config.get("background_pixels", 0))
            if (
                isinstance(weights, np.ndarray)
                and weights.shape == diff_mask.shape
                and total_weight > 0
            ):
                weighted_diff = float(weights[diff_mask].sum()) / total_weight * 100.0
                metadata_payload["weighted_diff_percentage"] = round(weighted_diff, 2)
                metadata_payload["weighted_diff_color"] = weight_config.get("color", WEIGHT_DIFF_COLOR)

                if (
                    isinstance(chrys_mask, np.ndarray)
                    and isinstance(background_mask, np.ndarray)
                    and chrys_mask.shape == diff_mask.shape
                    and background_mask.shape == diff_mask.shape
                ):
                    chrys_diff_pixels = int(np.count_nonzero(np.logical_and(diff_mask, chrys_mask)))
                    background_diff_pixels = int(np.count_nonzero(np.logical_and(diff_mask, background_mask)))
                    metadata_payload["chrysanthemum_diff_pixels"] = chrys_diff_pixels
                    metadata_payload["background_diff_pixels"] = background_diff_pixels

                metadata_payload["chrysanthemum_total_pixels"] = chrys_total_pixels
                metadata_payload["background_total_pixels"] = background_total_pixels
                metadata_payload["total_pixels"] = chrys_total_pixels + background_total_pixels

        # Create color-coded diff visualization
        diff_visual_img = None
        if weight_config:
            chrys_mask = weight_config.get("chrysanthemum_mask")
            background_mask = weight_config.get("background_mask")
            color_hex = weight_config.get("color", WEIGHT_DIFF_COLOR)
            try:
                purple_rgb = hex_to_rgb(color_hex)
            except ValueError:
                purple_rgb = (124, 58, 237)  # fallback purple

            if (
                isinstance(chrys_mask, np.ndarray)
                and isinstance(background_mask, np.ndarray)
                and chrys_mask.shape == diff_mask.shape
                and background_mask.shape == diff_mask.shape
            ):
                diff_visual = np.zeros((diff_mask.shape[0], diff_mask.shape[1], 4), dtype=np.uint8)
                chrys_diff = np.logical_and(diff_mask, chrys_mask)
                background_diff = np.logical_and(diff_mask, background_mask)
                other_diff = np.logical_and(diff_mask, ~(chrys_mask | background_mask))

                diff_visual[chrys_diff] = (*purple_rgb, 255)
                diff_visual[background_diff] = (0, 255, 0, 255)
                diff_visual[other_diff] = (0, 255, 0, 255)

                diff_visual_img = Image.fromarray(diff_visual, mode="RGBA")

        if diff_visual_img is None:
            diff_visual_img = diff_img.convert("RGBA")

        # Create the masked live image for display
        alpha_mask = ref_img.getchannel('A')
        live_with_mask = Image.new("RGBA", ref_img.size, (0, 0, 0, 0))
        live_with_mask.paste(aligned_live_img, mask=alpha_mask)

        live_image_message = create_image_message("live", img_to_bytes(live_with_mask.convert("RGBA")))
        diff_image_message = create_image_message("diff", img_to_bytes(diff_visual_img))

        return {
            "metadata": metadata_payload,
            "live_image_msg": live_image_message,
            "diff_image_msg": diff_image_message,
            "diff_pct": diff_pct, # For logging
            "changed_pixel_coord": changed_pixel_coord,
            "activity_payload": activity_payload,
            # "vandal_sample_coords": vandal_samples, # グリッドサンプリング廃止に伴い削除
        }
    except Exception as e:
        logging.error(f"Error during sync image processing: {e}", exc_info=True)
        return None

# ------------------ App State Management ------------------

class AppState:
    def __init__(self):
        self.lock = asyncio.Lock()
        self.latest_data: Dict[str, Any] = {
            "metadata": None,
            "live_image_msg": None,
            "diff_image_msg": None,
            "timestamp": 0,
            "vandal_sample_coords": [],
        }

app_state = AppState()

# SAMURAI monitor state
samurai_app_state = AppState()

class ConnectionManager:
    def __init__(self):
        self.active_connections: List[WebSocket] = []

    async def connect(self, websocket: WebSocket):
        await websocket.accept()
        self.active_connections.append(websocket)

    def disconnect(self, websocket: WebSocket):
        if websocket in self.active_connections:
            self.active_connections.remove(websocket)

    async def broadcast_json(self, data: dict):
        for connection in self.active_connections[:]:
            try:
                if connection.client_state == WebSocketState.CONNECTED:
                    await connection.send_json(data)
            except Exception:
                self.disconnect(connection)

    async def broadcast_bytes(self, data: bytes):
        for connection in self.active_connections[:]:
            try:
                if connection.client_state == WebSocketState.CONNECTED:
                    await connection.send_bytes(data)
            except Exception:
                self.disconnect(connection)

manager = ConnectionManager()
samurai_manager = ConnectionManager()
minimap_manager = ConnectionManager()
minimap_state = {"latest_image": None}
minimap_history = []  # List of {timestamp, filename, avg_diff}
MINIMAP_ARCHIVE_DIR = Path("/opt/wplace/minimap_archive")
MINIMAP_HISTORY_FILE = MINIMAP_ARCHIVE_DIR / "history.json"
MINIMAP_SAVE_INTERVAL = 300  # 5 minutes in seconds

# ------------------ Minimap History Persistence ------------------

def load_minimap_history():
    """Load minimap history from JSON file, or reconstruct from existing PNG files"""
    global minimap_history
    try:
        if MINIMAP_HISTORY_FILE.exists():
            with open(MINIMAP_HISTORY_FILE, 'r') as f:
                minimap_history = json.load(f)
            logging.info(f"[MINIMAP] Loaded {len(minimap_history)} history entries from file")
        else:
            # Try to reconstruct history from existing PNG files
            minimap_history = []
            if MINIMAP_ARCHIVE_DIR.exists():
                png_files = sorted(MINIMAP_ARCHIVE_DIR.glob("minimap_*.png"))
                for png_file in png_files:
                    try:
                        # Extract timestamp from filename: minimap_1759592467.png
                        timestamp_str = png_file.stem.split('_')[1]
                        timestamp = int(timestamp_str)
                        minimap_history.append({
                            "timestamp": timestamp,
                            "filename": png_file.name,
                            "avg_diff": 0.0  # Unknown, set to 0
                        })
                    except (IndexError, ValueError) as e:
                        logging.warning(f"[MINIMAP] Skipping invalid filename: {png_file.name}")
                        continue

                if minimap_history:
                    logging.info(f"[MINIMAP] Reconstructed {len(minimap_history)} history entries from PNG files")
                    # Save reconstructed history
                    save_minimap_history()
                else:
                    logging.info("[MINIMAP] No history file found, starting fresh")
            else:
                logging.info("[MINIMAP] No history file found, starting fresh")
    except Exception as e:
        logging.error(f"[MINIMAP] Failed to load history: {e}")
        minimap_history = []

def save_minimap_history():
    """Save minimap history to JSON file"""
    try:
        with open(MINIMAP_HISTORY_FILE, 'w') as f:
            json.dump(minimap_history, f, indent=2)
        logging.info(f"[MINIMAP] Saved {len(minimap_history)} history entries to file")
    except Exception as e:
        logging.error(f"[MINIMAP] Failed to save history: {e}")

# ------------------ Admin Helpers ------------------

def verify_admin_request(request: Request):
    if not ADMIN_API_TOKEN:
        raise HTTPException(status_code=503, detail="Admin API token not configured")
    token = request.headers.get("X-Admin-Token")
    if token != ADMIN_API_TOKEN:
        raise HTTPException(status_code=403, detail="Invalid admin token")

# ------------------ Background Tasks ------------------

async def fetch_tiles(start_tx: int, end_tx: int, start_ty: int, end_ty: int) -> Dict[Tuple[int, int], Image.Image]:
    """Asynchronously fetches all required tiles."""
    logging.info("[FETCH] Start")
    tasks = []
    tile_coords = []
    for tx in range(start_tx, end_tx + 1):
        for ty in range(start_ty, end_ty + 1):
            url = f"{TILES_BASE}/{tx}/{ty}.png?t={random.randint(1000, 9999)}"
            tasks.append(asyncio.to_thread(get_image_from_url, url))
            tile_coords.append((tx, ty))

    logging.info(f"[FETCH] Fetching {len(tasks)} tiles...")
    results = await asyncio.gather(*tasks)

    tiles = {}
    for i, im in enumerate(results):
        if im is not None:
            tx, ty = tile_coords[i]
            tiles[(tx, ty)] = im.convert("RGBA")
    logging.info(f"[FETCH] End, got {len(tiles)} tiles")
    return tiles

async def fetch_and_process_data_loop(executor: ProcessPoolExecutor):
    if REF_IMG is None:
        logging.critical("Cannot start processing loop: reference image not loaded")
        return

    loop = asyncio.get_running_loop()
    tile_x, tile_y, x_in_tile, y_in_tile = REF_PIXEL
    last_successful_data = None  # Cache for previous successful data

    while True:
        try:
            logging.info("[PROCESS] Loop start")

            global_x = tile_x * TILE_SIZE + x_in_tile
            global_y = tile_y * TILE_SIZE + y_in_tile
            start_tx = global_x // TILE_SIZE
            start_ty = global_y // TILE_SIZE
            end_tx = (global_x + MONITOR_W - 1) // TILE_SIZE
            end_ty = (global_y + MONITOR_H - 1) // TILE_SIZE

            logging.info("[PROCESS] Calling fetch_tiles")
            tiles = await fetch_tiles(start_tx, end_tx, start_ty, end_ty)
            logging.info(f"[PROCESS] fetch_tiles returned")

            # Check if we got enough tiles (should have all tiles in the range)
            expected_tile_count = (end_tx - start_tx + 1) * (end_ty - start_ty + 1)
            actual_tile_count = len(tiles)

            if tiles and actual_tile_count >= expected_tile_count:
                logging.info("[PROCESS] Calling stitch_and_crop_tiles in executor")
                live_img = await loop.run_in_executor(
                    executor, stitch_and_crop_tiles, tiles, start_tx, start_ty, end_tx, end_ty,
                    global_x, global_y, MONITOR_W, MONITOR_H, TILE_SIZE
                )
                logging.info("[PROCESS] stitch_and_crop_tiles returned")

                if live_img:
                    logging.info("[PROCESS] Calling process_live_image in executor")
                    processed_data = await loop.run_in_executor(
                        executor, process_live_image, REF_IMG, live_img, 45, 15, WEIGHT_CONFIG
                    )
                    logging.info("[PROCESS] process_live_image returned")

                    if processed_data:
                        schedule_activity_payload_processing(processed_data.get("activity_payload"))

                        async with app_state.lock:
                            app_state.latest_data["metadata"] = processed_data["metadata"]
                            app_state.latest_data["live_image_msg"] = processed_data["live_image_msg"]
                            app_state.latest_data["diff_image_msg"] = processed_data["diff_image_msg"]
                            app_state.latest_data["timestamp"] = time.time()

                            # 9秒ごとの問合せループ用にサンプル座標を追記
                            new_coords = processed_data.get("vandal_sample_coords", [])
                            if new_coords:
                                app_state.latest_data["vandal_sample_coords"].extend(new_coords)

                        last_successful_data = processed_data  # Cache successful data
                        diff_pct = processed_data["diff_pct"]
                        logging.info(f"[PROCESS] New data ready. Diff: {diff_pct:.2f}%")
                else:
                    # Failed to stitch tiles, send error message
                    logging.warning("[PROCESS] Failed to stitch tiles. Sending error message.")
                    async with app_state.lock:
                        app_state.latest_data = {
                            "metadata": {"type": "error", "message": "タイル取得失敗"},
                            "live_image_msg": None,
                            "diff_image_msg": None,
                            "timestamp": time.time(),
                            "vandal_sample_coords": [],
                        }
            else:
                # Failed to fetch all tiles, send error message
                logging.warning(f"[PROCESS] Failed to fetch all tiles ({actual_tile_count}/{expected_tile_count}). Sending error message.")
                async with app_state.lock:
                    app_state.latest_data = {
                        "metadata": {"type": "error", "message": "タイル取得失敗"},
                        "live_image_msg": None,
                        "diff_image_msg": None,
                        "timestamp": time.time(),
                        "vandal_sample_coords": [],
                    }

        except Exception as e:
            logging.error(f"[PROCESS] Error in async processing loop: {e}", exc_info=True)
            await asyncio.sleep(10)
        logging.info("[PROCESS] Loop end")
        await asyncio.sleep(1.0)

async def broadcast_loop():
    interval = 1.0 # Updated to 1.0 seconds (maximum speed)
    logging.info(f"[BROADCAST] Starting broadcast loop with interval: {interval:.2f}s")
    while True:
        logging.info(f"[BROADCAST] Loop start, sleeping for {interval:.2f}s")
        await asyncio.sleep(interval)
        logging.info("[BROADCAST] Woke up after sleep")

        current_data = None
        async with app_state.lock:
            current_data = app_state.latest_data.copy()

        if manager.active_connections and current_data and current_data["metadata"]:
            logging.info(f"[BROADCAST] Broadcasting to {len(manager.active_connections)} clients.")
            await manager.broadcast_json(current_data["metadata"])

            # Only broadcast images if they exist (not an error message)
            if current_data["live_image_msg"] and current_data["diff_image_msg"]:
                await manager.broadcast_bytes(current_data["live_image_msg"])
                await manager.broadcast_bytes(current_data["diff_image_msg"])

            logging.info("[BROADCAST] Broadcast complete")
        else:
            logging.info("[BROADCAST] No data or no clients, skipping broadcast")

# ------------------ FastAPI App ------------------

executor = ProcessPoolExecutor(max_workers=1)
samurai_executor = ProcessPoolExecutor(max_workers=1)
app = FastAPI()

async def samurai_fetch_and_process_loop_generic(name: str, samurai_data: dict, executor_pool: ProcessPoolExecutor, delay: float = 0):
    """Generic SAMURAI monitor processing loop for any location"""
    if samurai_data is None:
        logging.critical(f"[{name}] Cannot start processing loop: reference image not loaded")
        return

    # Initial delay to stagger requests
    if delay > 0:
        await asyncio.sleep(delay)

    ref_img = samurai_data["ref_img"]
    ref_pixel = samurai_data["ref_pixel"]
    app_state_obj = samurai_data["app_state"]
    monitor_w = samurai_data["monitor_w"]
    monitor_h = samurai_data["monitor_h"]

    loop = asyncio.get_running_loop()
    tile_x, tile_y, x_in_tile, y_in_tile = ref_pixel
    last_successful_data = None
    INTERVAL = 15  # 15 seconds interval

    while True:
        try:
            logging.info(f"[{name}-PROCESS] Loop start")

            global_x = tile_x * TILE_SIZE + x_in_tile
            global_y = tile_y * TILE_SIZE + y_in_tile
            start_tx = global_x // TILE_SIZE
            start_ty = global_y // TILE_SIZE
            end_tx = (global_x + monitor_w - 1) // TILE_SIZE
            end_ty = (global_y + monitor_h - 1) // TILE_SIZE

            tiles = await fetch_tiles(start_tx, end_tx, start_ty, end_ty)

            if tiles:
                live_img = await loop.run_in_executor(
                    executor_pool, stitch_and_crop_tiles, tiles, start_tx, start_ty, end_tx, end_ty,
                    global_x, global_y, monitor_w, monitor_h, TILE_SIZE
                )

                if live_img:
                    processed_data = await loop.run_in_executor(
                        executor_pool, process_live_image, ref_img, live_img, 150, 40, None
                    )

                    if processed_data:
                        async with app_state_obj.lock:
                            app_state_obj.latest_data = {
                                "metadata": processed_data["metadata"],
                                "live_image_msg": processed_data["live_image_msg"],
                                "diff_image_msg": processed_data["diff_image_msg"],
                                "timestamp": time.time(),
                            }

                        last_successful_data = processed_data
                        diff_pct = processed_data["diff_pct"]
                        logging.info(f"[{name}-PROCESS] New data ready. Diff: {diff_pct:.2f}%")
                else:
                    if last_successful_data:
                        logging.warning(f"[{name}-PROCESS] Failed to stitch tiles. Using previous data.")
                        async with app_state_obj.lock:
                            app_state_obj.latest_data = {
                                "metadata": last_successful_data["metadata"],
                                "live_image_msg": last_successful_data["live_image_msg"],
                                "diff_image_msg": last_successful_data["diff_image_msg"],
                                "timestamp": time.time(),
                            }
            else:
                if last_successful_data:
                    logging.warning(f"[{name}-PROCESS] Failed to fetch tiles. Using previous data.")
                    async with app_state_obj.lock:
                        app_state_obj.latest_data = {
                            "metadata": last_successful_data["metadata"],
                            "live_image_msg": last_successful_data["live_image_msg"],
                            "diff_image_msg": last_successful_data["diff_image_msg"],
                            "timestamp": time.time(),
                        }
                else:
                    logging.warning(f"[{name}-PROCESS] Failed to fetch any tiles and no previous data.")

        except Exception as e:
            logging.error(f"[{name}-PROCESS] Error in async processing loop: {e}", exc_info=True)

        # Wait for next interval
        await asyncio.sleep(INTERVAL)

async def samurai_fetch_and_process_loop():
    """SAMURAI monitor processing loop"""
    if SAMURAI_REF_IMG is None:
        logging.critical("Cannot start SAMURAI processing loop: reference image not loaded")
        return

    loop = asyncio.get_running_loop()
    tile_x, tile_y, x_in_tile, y_in_tile = SAMURAI_REF_PIXEL
    last_successful_data = None  # Cache for previous successful data

    while True:
        try:
            logging.info("[SAMURAI-PROCESS] Loop start")

            global_x = tile_x * TILE_SIZE + x_in_tile
            global_y = tile_y * TILE_SIZE + y_in_tile
            start_tx = global_x // TILE_SIZE
            start_ty = global_y // TILE_SIZE
            end_tx = (global_x + SAMURAI_MONITOR_W - 1) // TILE_SIZE
            end_ty = (global_y + SAMURAI_MONITOR_H - 1) // TILE_SIZE

            tiles = await fetch_tiles(start_tx, end_tx, start_ty, end_ty)

            if tiles:
                live_img = await loop.run_in_executor(
                    samurai_executor, stitch_and_crop_tiles, tiles, start_tx, start_ty, end_tx, end_ty,
                    global_x, global_y, SAMURAI_MONITOR_W, SAMURAI_MONITOR_H, TILE_SIZE
                )

                if live_img:
                    # Use lenient thresholds for SAMURAI (150 for RGB, 40 for alpha)
                    processed_data = await loop.run_in_executor(
                        samurai_executor, process_live_image, SAMURAI_REF_IMG, live_img, 150, 40, None
                    )

                    if processed_data:
                        async with samurai_app_state.lock:
                            samurai_app_state.latest_data = {
                                "metadata": processed_data["metadata"],
                                "live_image_msg": processed_data["live_image_msg"],
                                "diff_image_msg": processed_data["diff_image_msg"],
                                "timestamp": time.time(),
                            }

                        last_successful_data = processed_data  # Cache successful data
                        diff_pct = processed_data["diff_pct"]
                        logging.info(f"[SAMURAI-PROCESS] New data ready. Diff: {diff_pct:.2f}%")
                else:
                    # Failed to stitch tiles, use cached data if available
                    if last_successful_data:
                        logging.warning("[SAMURAI-PROCESS] Failed to stitch tiles. Using previous data.")
                        async with samurai_app_state.lock:
                            samurai_app_state.latest_data = {
                                "metadata": last_successful_data["metadata"],
                                "live_image_msg": last_successful_data["live_image_msg"],
                                "diff_image_msg": last_successful_data["diff_image_msg"],
                                "timestamp": time.time(),
                            }
            else:
                # Failed to fetch tiles, use cached data if available
                if last_successful_data:
                    logging.warning("[SAMURAI-PROCESS] Failed to fetch tiles. Using previous data.")
                    async with samurai_app_state.lock:
                        samurai_app_state.latest_data = {
                            "metadata": last_successful_data["metadata"],
                            "live_image_msg": last_successful_data["live_image_msg"],
                            "diff_image_msg": last_successful_data["diff_image_msg"],
                            "timestamp": time.time(),
                        }
                else:
                    logging.warning("[SAMURAI-PROCESS] Failed to fetch any tiles and no previous data. Retrying in 5s.")
                    await asyncio.sleep(5)

        except Exception as e:
            logging.error(f"[SAMURAI-PROCESS] Error in async processing loop: {e}", exc_info=True)
            await asyncio.sleep(10)

async def samurai_broadcast_loop_generic(name: str, samurai_data: dict, delay: float = 0):
    """Generic SAMURAI broadcast loop"""
    # Initial delay to match processing loop
    if delay > 0:
        await asyncio.sleep(delay)

    interval = 15.0
    app_state_obj = samurai_data["app_state"]
    manager_obj = samurai_data["manager"]

    logging.info(f"[{name}-BROADCAST] Starting broadcast loop with interval: {interval:.2f}s")
    while True:
        await asyncio.sleep(interval)

        current_data = None
        async with app_state_obj.lock:
            current_data = app_state_obj.latest_data.copy()

        if manager_obj.active_connections and current_data and current_data["metadata"]:
            logging.info(f"[{name}-BROADCAST] Broadcasting to {len(manager_obj.active_connections)} clients.")
            await manager_obj.broadcast_json(current_data["metadata"])
            await manager_obj.broadcast_bytes(current_data["live_image_msg"])
            await manager_obj.broadcast_bytes(current_data["diff_image_msg"])

async def minimap_fetch_loop():
    """Fetch and stitch minimap tiles every 60 seconds"""
    MAP_MIN_TX, MAP_MAX_TX = 860, 862  # 3 tiles wide
    MAP_MIN_TY, MAP_MAX_TY = 1201, 1203  # 3 tiles high

    logging.info("[MINIMAP] Starting minimap fetch loop")
    last_save_time = -MINIMAP_SAVE_INTERVAL  # Initialize to trigger save on first run

    while True:
        try:
            # Collect all tiles needed (3x3 = 9 tiles)
            all_tiles_needed = set()
            for tx in range(MAP_MIN_TX, MAP_MAX_TX + 1):
                for ty in range(MAP_MIN_TY, MAP_MAX_TY + 1):
                    all_tiles_needed.add((tx, ty))

            logging.info(f"[MINIMAP] Need {len(all_tiles_needed)} tiles")

            # Fetch tiles slowly over 60 seconds (60/9 ≈ 6.67 seconds per tile)
            tile_interval = 60.0 / len(all_tiles_needed)
            tiles = {}

            for i, (tx, ty) in enumerate(sorted(all_tiles_needed)):
                url = f"{TILES_BASE}/{tx}/{ty}.png?t={random.randint(1000, 9999)}"
                img = await asyncio.to_thread(get_image_from_url, url)
                if img:
                    tiles[(tx, ty)] = img
                    logging.info(f"[MINIMAP] Fetched tile ({tx},{ty}) - {i+1}/{len(all_tiles_needed)}")

                # Wait before fetching next tile (except for the last one)
                if i < len(all_tiles_needed) - 1:
                    await asyncio.sleep(tile_interval)

            # Stitch all tiles together
            if tiles:
                map_width = (MAP_MAX_TX - MAP_MIN_TX + 1) * TILE_SIZE
                map_height = (MAP_MAX_TY - MAP_MIN_TY + 1) * TILE_SIZE
                combined = Image.new("RGBA", (map_width, map_height))

                for (tx, ty), img in tiles.items():
                    px = (tx - MAP_MIN_TX) * TILE_SIZE
                    py = (ty - MAP_MIN_TY) * TILE_SIZE
                    combined.paste(img, (px, py))

                # Create message
                minimap_msg = create_image_message("minimap", img_to_bytes(combined))
                minimap_state["latest_image"] = minimap_msg

                # Save to disk every 5 minutes
                current_time = time.time()
                if current_time - last_save_time >= MINIMAP_SAVE_INTERVAL:
                    timestamp = int(current_time)
                    filename = f"minimap_{timestamp}.png"
                    filepath = MINIMAP_ARCHIVE_DIR / filename
                    combined.save(filepath, "PNG")

                    # Calculate average diff from all SAMURAI monitors
                    total_diff = 0
                    monitor_count = 0
                    for name, samurai_data in SAMURAI_DATA.items():
                        if samurai_data:
                            async with samurai_data["app_state"].lock:
                                data = samurai_data["app_state"].latest_data
                                if data and data.get("metadata"):
                                    total_diff += data["metadata"].get("diff_percentage", 0)
                                    monitor_count += 1

                    avg_diff = total_diff / monitor_count if monitor_count > 0 else 0

                    minimap_history.append({
                        "timestamp": timestamp,
                        "filename": filename,
                        "avg_diff": round(avg_diff, 2)
                    })

                    # ARCHIVE MODE: Auto-deletion disabled to preserve all history
                    # Keep only last 1200 entries (4 days at 5 min intervals = 1152, rounded up)
                    # if len(minimap_history) > 1200:
                    #     # Delete old file
                    #     old_entry = minimap_history.pop(0)
                    #     old_path = MINIMAP_ARCHIVE_DIR / old_entry["filename"]
                    #     if old_path.exists():
                    #         old_path.unlink()

                    # Save history to file
                    save_minimap_history()

                    last_save_time = current_time
                    logging.info(f"[MINIMAP] Saved to {filename}, avg_diff: {avg_diff:.2f}%, history: {len(minimap_history)} entries")

                # Broadcast to connected clients
                if minimap_manager.active_connections:
                    logging.info(f"[MINIMAP] Broadcasting to {len(minimap_manager.active_connections)} clients")
                    await minimap_manager.broadcast_bytes(minimap_msg)

                logging.info(f"[MINIMAP] Created map: {map_width}x{map_height}px with {len(tiles)} tiles")

        except Exception as e:
            logging.error(f"[MINIMAP] Error in fetch loop: {e}", exc_info=True)

        # After completing one cycle, wait a bit before starting next cycle
        await asyncio.sleep(5)

async def samurai_broadcast_loop():
    """SAMURAI monitor broadcast loop"""
    interval = 10.0
    logging.info(f"[SAMURAI-BROADCAST] Starting broadcast loop with interval: {interval:.2f}s")
    while True:
        await asyncio.sleep(interval)

        current_data = None
        async with samurai_app_state.lock:
            current_data = samurai_app_state.latest_data.copy()

        if samurai_manager.active_connections and current_data and current_data["metadata"]:
            logging.info(f"[SAMURAI-BROADCAST] Broadcasting to {len(samurai_manager.active_connections)} clients.")
            await samurai_manager.broadcast_json(current_data["metadata"])
            await samurai_manager.broadcast_bytes(current_data["live_image_msg"])
            await samurai_manager.broadcast_bytes(current_data["diff_image_msg"])





@app.on_event("startup")
async def startup_event():
    logging.info("Starting background tasks...")

    # Load minimap history from disk
    load_minimap_history()

    # ARCHIVE MODE: JFA event ended - SAMURAI monitoring disabled
    # BUT: Kiku (Imperial Palace) monitoring continues
    logging.info("=" * 60)
    logging.info("[ARCHIVE MODE] JFA event ended - SAMURAI monitoring disabled")
    logging.info("[ACTIVE] Kiku (Imperial Palace) monitoring: ENABLED")
    logging.info("[ACTIVE] Discord bot notifications: ENABLED")
    logging.info("[ARCHIVE MODE] JFA timeline and history viewing available")
    logging.info("=" * 60)

    # Continue monitoring Kiku (Imperial Palace)
    asyncio.create_task(fetch_and_process_data_loop(executor))
    asyncio.create_task(broadcast_loop())

    # JFA SAMURAI monitoring tasks disabled - archive mode only
    # asyncio.create_task(samurai_fetch_and_process_loop())
    # asyncio.create_task(samurai_broadcast_loop())
    # asyncio.create_task(minimap_fetch_loop())

    # Start 5 SAMURAI monitoring tasks with staggered delays (3 seconds apart)
    # delay = 0
    # for name, samurai_data in SAMURAI_DATA.items():
    #     if samurai_data is not None:
    #         asyncio.create_task(samurai_fetch_and_process_loop_generic(name, samurai_data, samurai_executor, delay))
    #         asyncio.create_task(samurai_broadcast_loop_generic(name, samurai_data, delay))
    #         logging.info(f"Started monitoring tasks for {name} with {delay}s delay")
    #         delay += 3  # Stagger by 3 seconds

@app.on_event("shutdown")
def shutdown_event():
    logging.info("Shutting down process pool...")
    executor.shutdown(wait=True)
    samurai_executor.shutdown(wait=True)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

try:
    REF_IMG = load_reference(REF_IMAGE_PATH)
    MONITOR_W, MONITOR_H = REF_IMG.size
    REF_IMG_BYTES = img_to_bytes(REF_IMG)
    WEIGHT_CONFIG = build_weight_config(REF_IMG)
    REF_ALPHA_MASK = np.array(REF_IMG)[:, :, 3] > 0
    logging.info(f"Reference loaded: {REF_IMAGE_PATH} size={REF_IMG.size}")
except Exception as e:
    logging.critical(e, exc_info=True)
    REF_IMG = None
    MONITOR_W, MONITOR_H = 0, 0
    REF_IMG_BYTES = b""
    WEIGHT_CONFIG = None
    REF_ALPHA_MASK = None

# SAMURAI monitor references - 5 locations
SAMURAI_CONFIGS = [
    {"name": "JFA1", "pixel": (862, 1202, 340, 386), "file": "JFA1.png"},
    {"name": "JFA2", "pixel": (861, 1202, 947, 501), "file": "JFA2.png"},
    {"name": "JFA3", "pixel": (861, 1202, 50, 348), "file": "JFA3.png"},
    {"name": "JFA4", "pixel": (862, 1202, 110, 30), "file": "JFA4.png"},
    {"name": "JFA5", "pixel": (860, 1202, 820, 600), "file": "JFA5.png"},
]

# Load all SAMURAI references
SAMURAI_DATA = {}
for config in SAMURAI_CONFIGS:
    try:
        ref_img = load_reference(config["file"])
        SAMURAI_DATA[config["name"]] = {
            "ref_pixel": config["pixel"],
            "ref_img": ref_img,
            "monitor_w": ref_img.size[0],
            "monitor_h": ref_img.size[1],
            "ref_img_bytes": img_to_bytes(ref_img),
            "app_state": AppState(),
            "manager": ConnectionManager(),
        }
        logging.info(f"{config['name']} Reference loaded: {config['file']} size={ref_img.size}")
    except Exception as e:
        logging.critical(f"{config['name']} ref error: {e}", exc_info=True)
        SAMURAI_DATA[config["name"]] = None

# Legacy SAMURAI variables for backward compatibility
SAMURAI_REF_PIXEL = (862, 1202, 340, 386)
try:
    SAMURAI_REF_IMG = load_reference("JFA1.png")
    SAMURAI_MONITOR_W, SAMURAI_MONITOR_H = SAMURAI_REF_IMG.size
    SAMURAI_REF_IMG_BYTES = img_to_bytes(SAMURAI_REF_IMG)
except Exception as e:
    SAMURAI_REF_IMG = None
    SAMURAI_MONITOR_W, SAMURAI_MONITOR_H = 0, 0
    SAMURAI_REF_IMG_BYTES = b""

HTML_FILE = Path("tools/wplace_monitor.html")

@app.get("/", response_class=HTMLResponse)
async def index():
    if HTML_FILE.exists():
        return HTML_FILE.read_text(encoding="utf-8")
    return HTMLResponse("<h1>wplace monitor backend is running</h1>", status_code=200)

@app.get("/wplace_monitor.html")
async def get_html():
    if HTML_FILE.exists():
        return FileResponse(str(HTML_FILE))
    return HTMLResponse("wplace_monitor.html not found", status_code=404)

@app.get("/api/minimap/history")
async def get_minimap_history():
    """Get minimap history timeline"""
    return {"history": minimap_history}

@app.get("/api/minimap/archive/{filename}")
async def get_minimap_archive(filename: str):
    """Get archived minimap image"""
    filepath = MINIMAP_ARCHIVE_DIR / filename
    if filepath.exists() and filepath.parent == MINIMAP_ARCHIVE_DIR:
        return FileResponse(filepath)
    return Response(content="Not found", status_code=404)

@app.get("/api/minimap/timelapse")
async def get_minimap_timelapse():
    """Get pre-generated timelapse video"""
    filepath = MINIMAP_ARCHIVE_DIR / "minimap_timelapse.mp4"
    if filepath.exists():
        return FileResponse(filepath, media_type="video/mp4", filename="minimap_timelapse.mp4")
    return Response(content="Timelapse video not yet generated", status_code=404)

@app.get("/api/minimap/metadata")
async def get_minimap_metadata():
    """Get metadata about the minimap tile range."""
    try:
        # Calculate tile range needed for all JFA locations
        min_tx, max_tx = float('inf'), -float('inf')
        min_ty, max_ty = float('inf'), -float('inf')

        for config in SAMURAI_CONFIGS:
            w, h, x, y = config["pixel"]
            start_tx = x // TILE_SIZE
            end_tx = (x + w - 1) // TILE_SIZE
            start_ty = y // TILE_SIZE
            end_ty = (y + h - 1) // TILE_SIZE

            min_tx = min(min_tx, start_tx)
            max_tx = max(max_tx, end_tx)
            min_ty = min(min_ty, start_ty)
            max_ty = max(max_ty, end_ty)

        return {
            "min_tx": min_tx,
            "min_ty": min_ty,
            "max_tx": max_tx,
            "max_ty": max_ty,
            "tile_size": TILE_SIZE,
            "width": (max_tx - min_tx + 1) * TILE_SIZE,
            "height": (max_ty - min_ty + 1) * TILE_SIZE,
        }
    except Exception as e:
        logging.error(f"Error getting minimap metadata: {e}", exc_info=True)
        return Response(content=str(e), status_code=500)

@app.get("/api/minimap")
async def get_minimap():
    """Fetch and stitch tiles to create a minimap covering all JFA locations."""
    try:
        # Calculate tile range needed for all JFA locations
        min_tx, max_tx = float('inf'), -float('inf')
        min_ty, max_ty = float('inf'), -float('inf')

        for config in SAMURAI_CONFIGS:
            w, h, x, y = config["pixel"]
            # Calculate tile coordinates
            start_tx = x // TILE_SIZE
            end_tx = (x + w - 1) // TILE_SIZE
            start_ty = y // TILE_SIZE
            end_ty = (y + h - 1) // TILE_SIZE

            min_tx = min(min_tx, start_tx)
            max_tx = max(max_tx, end_tx)
            min_ty = min(min_ty, start_ty)
            max_ty = max(max_ty, end_ty)

        logging.info(f"[MINIMAP] Fetching tiles from ({min_tx},{min_ty}) to ({max_tx},{max_ty})")

        # Fetch tiles
        tiles = await fetch_tiles(min_tx, max_tx, min_ty, max_ty)

        if not tiles:
            return Response(content="Failed to fetch tiles", status_code=500)

        # Stitch tiles together
        tile_cols = max_tx - min_tx + 1
        tile_rows = max_ty - min_ty + 1
        comb_w = tile_cols * TILE_SIZE
        comb_h = tile_rows * TILE_SIZE
        combined = Image.new("RGBA", (comb_w, comb_h))

        for (tx, ty), im in tiles.items():
            px = (tx - min_tx) * TILE_SIZE
            py = (ty - min_ty) * TILE_SIZE
            combined.paste(im, (px, py))

        # Add metadata as PNG text chunk
        from PIL import PngImagePlugin

        metadata = PngImagePlugin.PngInfo()
        metadata.add_text("min_tx", str(min_tx))
        metadata.add_text("min_ty", str(min_ty))
        metadata.add_text("max_tx", str(max_tx))
        metadata.add_text("max_ty", str(max_ty))

        # Convert to bytes and return as PNG
        buf = io.BytesIO()
        combined.save(buf, format="PNG", pnginfo=metadata)
        buf.seek(0)

        logging.info(f"[MINIMAP] Created minimap of size {comb_w}x{comb_h}, tiles ({min_tx},{min_ty}) to ({max_tx},{max_ty})")

        return Response(content=buf.getvalue(), media_type="image/png")
    except Exception as e:
        logging.error(f"Error creating minimap: {e}", exc_info=True)
        return Response(content=str(e), status_code=500)


@app.post("/api/admin/activity/clear")
async def admin_clear_activity(payload: Dict[str, Any], request: Request):
    verify_admin_request(request)
    painter_id = str(payload.get("painter_id", "")).strip()
    if not painter_id:
        raise HTTPException(status_code=400, detail="painter_id is required")

    result = await ACTIVITY_TRACKER.delete_painter(painter_id)
    status = "not_found"
    if result["removed_user"] or result["removed_pixels"]:
        status = "ok"
    return {"status": status, **result}

@app.websocket("/ws")
async def ws_endpoint(ws: WebSocket):
    await manager.connect(ws)
    logging.info(f"Client connected. Total clients: {len(manager.active_connections)}")
    try:
        if REF_IMG_BYTES:
            initial_ref_message = create_image_message("ref", REF_IMG_BYTES)
            await ws.send_bytes(initial_ref_message)

        async with app_state.lock:
            metadata = app_state.latest_data["metadata"]
            live_msg = app_state.latest_data["live_image_msg"]
            diff_msg = app_state.latest_data["diff_image_msg"]

        if metadata and live_msg and diff_msg:
            await ws.send_json(metadata)
            await ws.send_bytes(live_msg)
            await ws.send_bytes(diff_msg)
        else:
            await ws.send_json({"type": "status", "message": "Awaiting first data capture..."})

        while ws.client_state == WebSocketState.CONNECTED:
            await ws.receive_text()
    except WebSocketDisconnect:
        logging.info("Client disconnected.")
    finally:
        manager.disconnect(ws)
        logging.info(f"Client left. Total clients: {len(manager.active_connections)}")

@app.websocket("/ws/samurai")
async def ws_samurai_endpoint(ws: WebSocket):
    await samurai_manager.connect(ws)
    logging.info(f"[SAMURAI] Client connected. Total clients: {len(samurai_manager.active_connections)}")
    try:
        if SAMURAI_REF_IMG_BYTES:
            initial_ref_message = create_image_message("ref", SAMURAI_REF_IMG_BYTES)
            await ws.send_bytes(initial_ref_message)

        async with samurai_app_state.lock:
            metadata = samurai_app_state.latest_data["metadata"]
            live_msg = samurai_app_state.latest_data["live_image_msg"]
            diff_msg = samurai_app_state.latest_data["diff_image_msg"]

        if metadata and live_msg and diff_msg:
            await ws.send_json(metadata)
            await ws.send_bytes(live_msg)
            await ws.send_bytes(diff_msg)
        else:
            await ws.send_json({"type": "status", "message": "Awaiting first data capture..."})

        while ws.client_state == WebSocketState.CONNECTED:
            await ws.receive_text()
    except WebSocketDisconnect:
        logging.info("[SAMURAI] Client disconnected.")
    finally:
        samurai_manager.disconnect(ws)
        logging.info(f"[SAMURAI] Client left. Total clients: {len(samurai_manager.active_connections)}")

# 5 SAMURAI WebSocket endpoints
@app.websocket("/ws/jfa1")
async def ws_jfa1_endpoint(ws: WebSocket):
    await ws_samurai_generic(ws, "JFA1")

@app.websocket("/ws/jfa2")
async def ws_jfa2_endpoint(ws: WebSocket):
    await ws_samurai_generic(ws, "JFA2")

@app.websocket("/ws/jfa3")
async def ws_jfa3_endpoint(ws: WebSocket):
    await ws_samurai_generic(ws, "JFA3")

@app.websocket("/ws/jfa4")
async def ws_jfa4_endpoint(ws: WebSocket):
    await ws_samurai_generic(ws, "JFA4")

@app.websocket("/ws/jfa5")
async def ws_jfa5_endpoint(ws: WebSocket):
    await ws_samurai_generic(ws, "JFA5")

@app.websocket("/ws/minimap")
async def ws_minimap_endpoint(ws: WebSocket):
    await minimap_manager.connect(ws)
    logging.info(f"[MINIMAP] Client connected. Total clients: {len(minimap_manager.active_connections)}")

    try:
        # Send latest minimap if available
        if minimap_state["latest_image"]:
            await ws.send_bytes(minimap_state["latest_image"])

        # Keep connection alive
        while True:
            try:
                await ws.receive_text()
            except WebSocketDisconnect:
                break
    finally:
        minimap_manager.disconnect(ws)
        logging.info(f"[MINIMAP] Client disconnected. Total clients: {len(minimap_manager.active_connections)}")

async def ws_samurai_generic(ws: WebSocket, name: str):
    """Generic SAMURAI WebSocket endpoint"""
    samurai_data = SAMURAI_DATA.get(name)
    if samurai_data is None:
        await ws.close(code=1011, reason=f"{name} not loaded")
        return

    manager_obj = samurai_data["manager"]
    app_state_obj = samurai_data["app_state"]
    ref_img_bytes = samurai_data["ref_img_bytes"]

    await manager_obj.connect(ws)
    logging.info(f"[{name}] Client connected. Total clients: {len(manager_obj.active_connections)}")
    try:
        if ref_img_bytes:
            initial_ref_message = create_image_message("ref", ref_img_bytes)
            await ws.send_bytes(initial_ref_message)

        async with app_state_obj.lock:
            metadata = app_state_obj.latest_data.get("metadata")
            live_msg = app_state_obj.latest_data.get("live_image_msg")
            diff_msg = app_state_obj.latest_data.get("diff_image_msg")

        if metadata and live_msg and diff_msg:
            await ws.send_json(metadata)
            await ws.send_bytes(live_msg)
            await ws.send_bytes(diff_msg)
        else:
            await ws.send_json({"type": "status", "message": "Awaiting first data capture..."})

        while ws.client_state == WebSocketState.CONNECTED:
            await ws.receive_text()
    except WebSocketDisconnect:
        logging.info(f"[{name}] Client disconnected.")
    finally:
        manager_obj.disconnect(ws)
        logging.info(f"[{name}] Client left. Total clients: {len(manager_obj.active_connections)}")

if __name__ == "__main__":
    import uvicorn
    host = os.getenv("UVICORN_HOST", "127.0.0.1")
    port = int(os.getenv("UVICORN_PORT", "8000"))
    uvicorn.run("server_new:app", host=host, port=port, reload=False, ws="websockets", log_config=None)
