package monitor

import (
	"image"
	"image/color"
	"testing"
)

func solidNRGBA(w, h int, c color.NRGBA) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return img
}

func TestReferenceCompareThresholds(t *testing.T) {
	t.Parallel()

	base := color.NRGBA{R: 100, G: 100, B: 100, A: 255}
	tmpl := solidNRGBA(6, 1, base)
	tmpl.SetNRGBA(5, 0, color.NRGBA{}) // テンプレートの透明部分は対象外
	ref, err := newReference(tmpl)
	if err != nil {
		t.Fatalf("newReference returned error: %v", err)
	}

	live := solidNRGBA(6, 1, base)
	live.SetNRGBA(1, 0, color.NRGBA{R: 115, G: 115, B: 115, A: 255}) // RGB差の合計45: 差分ではない
	live.SetNRGBA(2, 0, color.NRGBA{R: 115, G: 115, B: 116, A: 255}) // RGB差の合計46: 差分
	live.SetNRGBA(3, 0, color.NRGBA{R: 100, G: 100, B: 100, A: 240}) // アルファ差15: 差分ではない
	live.SetNRGBA(4, 0, color.NRGBA{R: 100, G: 100, B: 100, A: 239}) // アルファ差16: 差分
	live.SetNRGBA(5, 0, color.NRGBA{R: 255, A: 255})                 // テンプレート透明部分の変化

	res, err := ref.compare(live)
	if err != nil {
		t.Fatalf("compare returned error: %v", err)
	}
	if res.data.DiffPixels != 2 || res.data.TotalPixels != 5 {
		t.Fatalf("unexpected counts: diff=%d total=%d", res.data.DiffPixels, res.data.TotalPixels)
	}
	if res.data.DiffPercentage != 40 {
		t.Fatalf("DiffPercentage mismatch: got %.2f", res.data.DiffPercentage)
	}
	for x, want := range []bool{false, false, true, false, true, false} {
		if got := res.diff.NRGBAAt(x, 0).A != 0; got != want {
			t.Fatalf("diff pixel x=%d: got %t want %t", x, got, want)
		}
	}
	if got := res.diff.NRGBAAt(2, 0); got != diffColor {
		t.Fatalf("diff color mismatch: %+v", got)
	}
	if got := res.live.NRGBAAt(5, 0); got.A != 0 {
		t.Fatalf("live image should be masked by template alpha: %+v", got)
	}
}

func TestReferenceCompareErasedBlackPixel(t *testing.T) {
	t.Parallel()

	// 黒いピクセルを消すと RGB は (0,0,0) のまま透明になる。アルファ差で検出できること。
	ref, err := newReference(solidNRGBA(2, 1, color.NRGBA{A: 255}))
	if err != nil {
		t.Fatalf("newReference returned error: %v", err)
	}
	live := solidNRGBA(2, 1, color.NRGBA{A: 255})
	live.SetNRGBA(1, 0, color.NRGBA{})

	res, err := ref.compare(live)
	if err != nil {
		t.Fatalf("compare returned error: %v", err)
	}
	if res.data.DiffPixels != 1 || res.data.DiffPercentage != 50 {
		t.Fatalf("erased pixel was not detected: %+v", res.data)
	}
}

func TestReferenceCompareSizeMismatch(t *testing.T) {
	t.Parallel()

	ref, err := newReference(solidNRGBA(2, 2, color.NRGBA{A: 255}))
	if err != nil {
		t.Fatalf("newReference returned error: %v", err)
	}
	if _, err := ref.compare(solidNRGBA(3, 2, color.NRGBA{A: 255})); err == nil {
		t.Fatal("expected size mismatch error")
	}
}
