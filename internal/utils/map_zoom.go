package utils

import "math"

const (
	// Web Mercator tile size used for zoom math.
	//
	// Note: Although MapLibre/Mapbox GL often uses 512px internally, Wplace's
	// observed "zoom=" behavior on wplace.live aligns with the classic 256px
	// tile size (it matches desktop verification for typical /get sizes).
	webMercatorTileSize = 256.0
	// Assumed viewport for shared links (desktop-first).
	defaultViewportWidth  = 1280.0
	defaultViewportHeight = 720.0
	// Wplace UI reduces effective visible map area from raw viewport.
	uiWidthFactor  = 0.82
	uiHeightFactor = 0.90
	// Calibrates Wplace zoom behavior against desktop observations.
	zoomBias = -0.43
	// Wplace visibility floor (canvas tiles can disappear below this on wplace.live).
	minCanvasZoom = 10.7
	maxSafeZoom   = 22.0
)

// ZoomFromImageSizeRaw calculates a deterministic MapLibre/WebMercator zoom that
// fits an area into a default viewport, based on map geometry (not regression).
//
// This is a "raw" map zoom. It can be < 10.7 for very large areas, which may
// cause the Wplace canvas layer to be hidden on wplace.live.
func ZoomFromImageSizeRaw(width, height int) float64 {
	if width <= 0 || height <= 0 {
		return minCanvasZoom
	}

	worldCanvasPx := float64(WplaceTilesPerEdge * WplaceTileSize)
	fracW := float64(width) / worldCanvasPx
	fracH := float64(height) / worldCanvasPx
	if fracW <= 0 || fracH <= 0 {
		return minCanvasZoom
	}

	usableW := defaultViewportWidth * uiWidthFactor
	usableH := defaultViewportHeight * uiHeightFactor
	if usableW <= 0 || usableH <= 0 {
		return minCanvasZoom
	}

	zoomW := math.Log2(usableW / (webMercatorTileSize * fracW))
	zoomH := math.Log2(usableH / (webMercatorTileSize * fracH))
	zoom := math.Min(zoomW, zoomH) + zoomBias

	if math.IsNaN(zoom) || math.IsInf(zoom, 0) {
		return minCanvasZoom
	}
	if zoom > maxSafeZoom {
		return maxSafeZoom
	}
	return zoom
}

// ZoomFromImageSizeCanvasSafe clamps the raw zoom to keep the Wplace canvas
// layer visible on wplace.live.
func ZoomFromImageSizeCanvasSafe(width, height int) float64 {
	zoom := ZoomFromImageSizeRaw(width, height)
	if zoom < minCanvasZoom {
		return minCanvasZoom
	}
	return zoom
}

// ZoomFromImageSize is kept for backward compatibility: it returns a canvas-safe
// zoom suitable for wplace.live links.
func ZoomFromImageSize(width, height int) float64 {
	return ZoomFromImageSizeCanvasSafe(width, height)
}
