package utils

import (
	"fmt"
	"math"
)

const (
	// WplaceZoom Wplaceのズームレベル
	WplaceZoom = 11
	// WplaceTileSize タイル1枚のサイズ (px)
	WplaceTileSize = 1000
	// WplaceTilesPerEdge 1辺のタイル数 = 2^zoom
	WplaceTilesPerEdge = 1 << WplaceZoom // 2048
	// WplaceHighDetailZoom is used when a link should focus on a single pixel.
	// Keep this in sync with `/me` link-flow behavior.
	WplaceHighDetailZoom = 21.17
)

// Coordinate 座標データ
type Coordinate struct {
	TileX  int
	TileY  int
	PixelX int
	PixelY int
}

// LngLat 経度緯度
type LngLat struct {
	Lng float64
	Lat float64
}

// LngLatToTilePixel 経度緯度からタイル座標とピクセル座標を計算
func LngLatToTilePixel(lng, lat float64) *Coordinate {
	n := float64(WplaceTilesPerEdge)

	// 経度からX座標
	tileXFloat := (lng + 180) / 360 * n
	tileX := int(tileXFloat)
	pixelX := int((tileXFloat - float64(tileX)) * WplaceTileSize)

	// 緯度からY座標（Webメルカトル投影）
	latRad := lat * math.Pi / 180
	tileYFloat := (1 - math.Asinh(math.Tan(latRad))/math.Pi) / 2 * n
	tileY := int(tileYFloat)
	pixelY := int((tileYFloat - float64(tileY)) * WplaceTileSize)

	return &Coordinate{
		TileX:  tileX,
		TileY:  tileY,
		PixelX: pixelX,
		PixelY: pixelY,
	}
}

// TilePixelToLngLatFloat float版（ピクセル中心などで利用）
func TilePixelToLngLatFloat(tileX, tileY int, pixelX, pixelY float64) *LngLat {
	n := float64(WplaceTilesPerEdge)
	xFloat := float64(tileX) + pixelX/float64(WplaceTileSize)
	yFloat := float64(tileY) + pixelY/float64(WplaceTileSize)
	lng := xFloat/n*360 - 180
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*yFloat/n)))
	lat := latRad * 180 / math.Pi
	return &LngLat{
		Lng: lng,
		Lat: lat,
	}
}

// TilePixelCenterToLngLat ピクセル中心を指す経度緯度を返す
func TilePixelCenterToLngLat(tileX, tileY, pixelX, pixelY int) *LngLat {
	return TilePixelToLngLatFloat(tileX, tileY, float64(pixelX)+0.5, float64(pixelY)+0.5)
}

// BuildWplaceURL Wplace.liveのURLを生成
func BuildWplaceURL(lng, lat, zoom float64) string {
	return fmt.Sprintf("https://wplace.live/?lat=%.6f&lng=%.6f&zoom=%.2f",
		lat, lng, zoom)
}

// BuildWplacePixelURL returns a Wplace URL centered on a pixel coordinate.
func BuildWplacePixelURL(coord *Coordinate, zoom float64) string {
	if coord == nil {
		return ""
	}
	center := TilePixelCenterToLngLat(coord.TileX, coord.TileY, coord.PixelX, coord.PixelY)
	return BuildWplaceURL(center.Lng, center.Lat, zoom)
}

// BuildWplaceHighDetailPixelURL returns a high-zoom Wplace URL centered on a pixel.
func BuildWplaceHighDetailPixelURL(coord *Coordinate) string {
	return BuildWplacePixelURL(coord, WplaceHighDetailZoom)
}

// FormatHyphenCoords ハイフン形式の座標文字列を生成
func FormatHyphenCoords(coord *Coordinate) string {
	return fmt.Sprintf("%d-%d-%d-%d",
		coord.TileX, coord.TileY, coord.PixelX, coord.PixelY)
}
