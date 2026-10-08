package monitor

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"

	_ "image/png"
)

const (
	// diffRGBThreshold RGB各チャンネルの差の合計がこの値を超えたら差分とみなす
	diffRGBThreshold = 45
	// diffAlphaThreshold アルファ値の差がこの値を超えたら差分とみなす
	diffAlphaThreshold = 15
)

// diffColor 差分ピクセルの表示色（追加監視の差分画像と同じ赤）
var diffColor = color.NRGBA{R: 255, G: 0, B: 0, A: 255}

// reference 監視対象のテンプレート画像
type reference struct {
	img         *image.NRGBA
	opaqueCount int
}

// comparison テンプレートと現在の画像の比較結果
type comparison struct {
	data *MonitorData
	live *image.NRGBA
	diff *image.NRGBA
}

func loadReference(templatePath string) (*reference, error) {
	img, err := decodeImageFile(templatePath)
	if err != nil {
		return nil, fmt.Errorf("failed to load template %s: %w", templatePath, err)
	}
	ref, err := newReference(img)
	if err != nil {
		return nil, fmt.Errorf("invalid template %s: %w", templatePath, err)
	}
	return ref, nil
}

func decodeImageFile(path string) (*image.NRGBA, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(out, out.Bounds(), img, b.Min, draw.Src)
	return out, nil
}

func newReference(img *image.NRGBA) (*reference, error) {
	ref := &reference{img: img}
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] > 0 {
			ref.opaqueCount++
		}
	}
	if ref.opaqueCount == 0 {
		return nil, fmt.Errorf("template has no opaque pixels")
	}
	return ref, nil
}

// compare はテンプレートの不透明部分について live との差分を計算する。
// live はテンプレートと同じサイズであること。
func (r *reference) compare(live *image.NRGBA) (*comparison, error) {
	w, h := r.img.Bounds().Dx(), r.img.Bounds().Dy()
	if live.Bounds().Dx() != w || live.Bounds().Dy() != h {
		return nil, fmt.Errorf("live image size %dx%d does not match template %dx%d", live.Bounds().Dx(), live.Bounds().Dy(), w, h)
	}

	maskedLive := image.NewNRGBA(image.Rect(0, 0, w, h))
	diffImg := image.NewNRGBA(image.Rect(0, 0, w, h))
	diffPixels := 0

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			ti := y*r.img.Stride + x*4
			if r.img.Pix[ti+3] == 0 {
				continue
			}
			li := y*live.Stride + x*4
			oi := y*maskedLive.Stride + x*4
			copy(maskedLive.Pix[oi:oi+4], live.Pix[li:li+4])

			rgbDiff := absDiff(r.img.Pix[ti], live.Pix[li]) +
				absDiff(r.img.Pix[ti+1], live.Pix[li+1]) +
				absDiff(r.img.Pix[ti+2], live.Pix[li+2])
			alphaDiff := absDiff(r.img.Pix[ti+3], live.Pix[li+3])
			if rgbDiff <= diffRGBThreshold && alphaDiff <= diffAlphaThreshold {
				continue
			}

			diffPixels++
			diffImg.SetNRGBA(x, y, diffColor)
		}
	}

	data := &MonitorData{
		DiffPercentage: round2(float64(diffPixels) / float64(r.opaqueCount) * 100),
		DiffPixels:     diffPixels,
		TotalPixels:    r.opaqueCount,
	}
	return &comparison{data: data, live: maskedLive, diff: diffImg}, nil
}

func absDiff(a, b uint8) int {
	if a > b {
		return int(a - b)
	}
	return int(b - a)
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}
