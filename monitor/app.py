#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
Web backend for Wplace Vandalism Monitor (Web).
- Linux-first, no GUI deps
- FastAPI + WebSocket
- Periodically fetches tiles from wplace backend, crops area defined by REF_PIXEL
- Compares with reference image (PNG with alpha for mask) and streams base64 images + diff%
- Clients (the Discord bot) connect to ws://<host>:8000/ws

Environment variables (optional):
  REF_IMAGE_PATH  (default: kiku.png)  # PNG with alpha channel as mask; same semantics as 最新版.py
  REF_PIXEL       (default: "1818,806,989,359")  # tile_x, tile_y, x_in_tile, y_in_tile
  TILE_SIZE       (default: 1000)
"""

import os
import io
import asyncio
import time
import logging
import struct # Added for binary packing
import random
from pathlib import Path
from typing import Tuple, List, Dict, Any, Optional
from concurrent.futures import ProcessPoolExecutor

import requests
from PIL import Image
import numpy as np

from fastapi import FastAPI, WebSocket, WebSocketDisconnect
from fastapi.responses import HTMLResponse
from fastapi.middleware.cors import CORSMiddleware
from starlette.websockets import WebSocketState

# ------------------ Logging Config ------------------
logging.basicConfig(level=logging.INFO, format='%(asctime)s - %(levelname)s - %(message)s')

# ------------------ Config ------------------

REF_IMAGE_PATH = os.getenv("REF_IMAGE_PATH", "kiku.png")
REF_PIXEL_TEXT = os.getenv("REF_PIXEL", "1818,806,989,358")
# INTERVAL_MS is now hardcoded in the broadcast loop
TILE_SIZE = int(os.getenv("TILE_SIZE", "1000"))
TILES_BASE = os.getenv("TILES_BASE", "https://backend.wplace.live/files/s0/tiles")
WEIGHT_MASK_PATH = os.getenv("WEIGHT_MASK_PATH", "wplace_kiku_weight_mask.webp")
WEIGHT_DIFF_COLOR = "#7C3AED"  # purple


# ------------------ Utils (must be top-level for multiprocessing) ------------------

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
# --- Image Message Protocol ---
IMAGE_TYPE_REF = 1
IMAGE_TYPE_LIVE = 2
IMAGE_TYPE_DIFF = 3
IMAGE_TYPE_MAP = {"ref": IMAGE_TYPE_REF, "live": IMAGE_TYPE_LIVE, "diff": IMAGE_TYPE_DIFF}

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
        }

app_state = AppState()

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

# ------------------ Background Tasks ------------------

async def fetch_tiles(start_tx: int, end_tx: int, start_ty: int, end_ty: int) -> Dict[Tuple[int, int], Image.Image]:
    """Asynchronously fetches all required tiles."""
    logging.debug("[FETCH] Start")
    tasks = []
    tile_coords = []
    for tx in range(start_tx, end_tx + 1):
        for ty in range(start_ty, end_ty + 1):
            url = f"{TILES_BASE}/{tx}/{ty}.png?t={random.randint(1000, 9999)}"
            tasks.append(asyncio.to_thread(get_image_from_url, url))
            tile_coords.append((tx, ty))

    logging.debug(f"[FETCH] Fetching {len(tasks)} tiles...")
    results = await asyncio.gather(*tasks)

    tiles = {}
    for i, im in enumerate(results):
        if im is not None:
            tx, ty = tile_coords[i]
            tiles[(tx, ty)] = im.convert("RGBA")
    logging.debug(f"[FETCH] End, got {len(tiles)} tiles")
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
            logging.debug("[PROCESS] Loop start")

            global_x = tile_x * TILE_SIZE + x_in_tile
            global_y = tile_y * TILE_SIZE + y_in_tile
            start_tx = global_x // TILE_SIZE
            start_ty = global_y // TILE_SIZE
            end_tx = (global_x + MONITOR_W - 1) // TILE_SIZE
            end_ty = (global_y + MONITOR_H - 1) // TILE_SIZE

            logging.debug("[PROCESS] Calling fetch_tiles")
            tiles = await fetch_tiles(start_tx, end_tx, start_ty, end_ty)
            logging.debug(f"[PROCESS] fetch_tiles returned")

            # Check if we got enough tiles (should have all tiles in the range)
            expected_tile_count = (end_tx - start_tx + 1) * (end_ty - start_ty + 1)
            actual_tile_count = len(tiles)

            if tiles and actual_tile_count >= expected_tile_count:
                logging.debug("[PROCESS] Calling stitch_and_crop_tiles in executor")
                live_img = await loop.run_in_executor(
                    executor, stitch_and_crop_tiles, tiles, start_tx, start_ty, end_tx, end_ty,
                    global_x, global_y, MONITOR_W, MONITOR_H, TILE_SIZE
                )
                logging.debug("[PROCESS] stitch_and_crop_tiles returned")

                if live_img:
                    logging.debug("[PROCESS] Calling process_live_image in executor")
                    processed_data = await loop.run_in_executor(
                        executor, process_live_image, REF_IMG, live_img, 45, 15, WEIGHT_CONFIG
                    )
                    logging.debug("[PROCESS] process_live_image returned")

                    if processed_data:
                        async with app_state.lock:
                            app_state.latest_data["metadata"] = processed_data["metadata"]
                            app_state.latest_data["live_image_msg"] = processed_data["live_image_msg"]
                            app_state.latest_data["diff_image_msg"] = processed_data["diff_image_msg"]
                            app_state.latest_data["timestamp"] = time.time()

                        last_successful_data = processed_data  # Cache successful data
                        diff_pct = processed_data["diff_pct"]
                        logging.debug(f"[PROCESS] New data ready. Diff: {diff_pct:.2f}%")
                else:
                    # Failed to stitch tiles, send error message
                    logging.warning("[PROCESS] Failed to stitch tiles. Sending error message.")
                    async with app_state.lock:
                        app_state.latest_data = {
                            "metadata": {"type": "error", "message": "タイル取得失敗"},
                            "live_image_msg": None,
                            "diff_image_msg": None,
                            "timestamp": time.time(),
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
                    }

        except Exception as e:
            logging.error(f"[PROCESS] Error in async processing loop: {e}", exc_info=True)
            await asyncio.sleep(10)
        logging.debug("[PROCESS] Loop end")
        await asyncio.sleep(5.0)

async def broadcast_loop():
    interval = 5.0 # Updated to 1.0 seconds (maximum speed)
    logging.info(f"[BROADCAST] Starting broadcast loop with interval: {interval:.2f}s")
    while True:
        logging.debug(f"[BROADCAST] Loop start, sleeping for {interval:.2f}s")
        await asyncio.sleep(interval)
        logging.debug("[BROADCAST] Woke up after sleep")

        current_data = None
        async with app_state.lock:
            current_data = app_state.latest_data.copy()

        if manager.active_connections and current_data and current_data["metadata"]:
            logging.debug(f"[BROADCAST] Broadcasting to {len(manager.active_connections)} clients.")
            await manager.broadcast_json(current_data["metadata"])

            # Only broadcast images if they exist (not an error message)
            if current_data["live_image_msg"] and current_data["diff_image_msg"]:
                await manager.broadcast_bytes(current_data["live_image_msg"])
                await manager.broadcast_bytes(current_data["diff_image_msg"])

            logging.debug("[BROADCAST] Broadcast complete")
        else:
            logging.debug("[BROADCAST] No data or no clients, skipping broadcast")

# ------------------ FastAPI App ------------------

executor = ProcessPoolExecutor(max_workers=1)
app = FastAPI()





@app.on_event("startup")
async def startup_event():
    logging.info("Starting background tasks...")

    # BUT: Kiku (Imperial Palace) monitoring continues
    logging.info("=" * 60)
    logging.info("[ACTIVE] Kiku (Imperial Palace) monitoring: ENABLED")
    logging.info("[ACTIVE] Discord bot notifications: ENABLED")
    logging.info("=" * 60)

    # Continue monitoring Kiku (Imperial Palace)
    asyncio.create_task(fetch_and_process_data_loop(executor))
    asyncio.create_task(broadcast_loop())

@app.on_event("shutdown")
def shutdown_event():
    logging.info("Shutting down process pool...")
    executor.shutdown(wait=True)

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
    logging.info(f"Reference loaded: {REF_IMAGE_PATH} size={REF_IMG.size}")
except Exception as e:
    logging.critical(e, exc_info=True)
    REF_IMG = None
    MONITOR_W, MONITOR_H = 0, 0
    REF_IMG_BYTES = b""
    WEIGHT_CONFIG = None

@app.get("/", response_class=HTMLResponse)
async def index():
    return HTMLResponse("<h1>wplace monitor backend is running</h1>", status_code=200)

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

if __name__ == "__main__":
    import uvicorn
    host = os.getenv("UVICORN_HOST", "0.0.0.0")
    port = int(os.getenv("UVICORN_PORT", "8000"))
    uvicorn.run(app, host=host, port=port, reload=False, ws="websockets", log_config=None)
