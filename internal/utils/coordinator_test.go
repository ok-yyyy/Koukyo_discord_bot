package utils

import (
	"testing"
)

func TestCoordinateConversion_RoundTrip(t *testing.T) {
	// テストケース: 皇居周辺の座標（概算）など
	tests := []struct {
		name string
		lng  float64
		lat  float64
	}{
		{"Tokyo Station", 139.767125, 35.681236},
		{"Koukyo", 139.7528, 35.6852},
		{"Null Island", 0.0, 0.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 1. LngLat -> Pixel
			coord := LngLatToTilePixel(tt.lng, tt.lat)

			// 2. Pixel -> LngLat (復元)
			// Pixelの中心(0.5, 0.5)ではなく、左上(0,0)から計算して戻す場合の誤差を考慮
			// TilePixelToLngLatFloat は float計算をしているので、大きな誤差は出ないはずだが
			// 入力が float(LngLat) -> int(Pixel) なので量子化誤差が出る。

			// 逆変換して検証
			// ここでは変換ロジックが破綻していないか（極端な値にならないか）を確認
			restored := TilePixelToLngLatFloat(coord.TileX, coord.TileY, float64(coord.PixelX), float64(coord.PixelY))

			// 許容誤差 (度)。
			// WplaceZoom=11 (2048 tiles * 1000px = 2,048,000 px width)
			// 360 / 2,048,000 ≈ 0.000175 度/pixel
			// 量子化誤差を考慮して少し余裕を持たせる
			const tolerance = 0.00025

			if abs(tt.lng-restored.Lng) > tolerance {
				t.Errorf("Lng mismatch: got %v, want %v (diff %v)", restored.Lng, tt.lng, abs(tt.lng-restored.Lng))
			}
			if abs(tt.lat-restored.Lat) > tolerance {
				t.Errorf("Lat mismatch: got %v, want %v (diff %v)", restored.Lat, tt.lat, abs(tt.lat-restored.Lat))
			}
		})
	}
}

func abs(a float64) float64 {
	if a < 0 {
		return -a
	}
	return a
}
