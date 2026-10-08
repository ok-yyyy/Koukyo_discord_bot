package notifications

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"io"

	"Koukyo_discord_bot/internal/embeds"
)

func applyTemplateAlphaMask(templateImg *image.NRGBA, live *image.NRGBA) *image.NRGBA {
	out := image.NewNRGBA(live.Bounds())
	if templateImg == nil || live == nil {
		return out
	}
	for y := 0; y < templateImg.Bounds().Dy(); y++ {
		for x := 0; x < templateImg.Bounds().Dx(); x++ {
			ti := y*templateImg.Stride + x*4
			if templateImg.Pix[ti+3] == 0 {
				continue
			}
			li := y*live.Stride + x*4
			oi := y*out.Stride + x*4
			out.Pix[oi] = live.Pix[li]
			out.Pix[oi+1] = live.Pix[li+1]
			out.Pix[oi+2] = live.Pix[li+2]
			out.Pix[oi+3] = 255
		}
	}
	return out
}

func buildDiffMask(templateImg *image.NRGBA, live *image.NRGBA) (int, *image.NRGBA) {
	if templateImg == nil || live == nil {
		return 0, image.NewNRGBA(image.Rect(0, 0, 1, 1))
	}
	mask := image.NewNRGBA(image.Rect(0, 0, templateImg.Bounds().Dx(), templateImg.Bounds().Dy()))
	if live.Bounds().Dx() != templateImg.Bounds().Dx() || live.Bounds().Dy() != templateImg.Bounds().Dy() {
		fillOpaqueMask(mask, templateImg)
		return countOpaque(templateImg), mask
	}
	diff := 0
	diffColor := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
	for y := 0; y < templateImg.Bounds().Dy(); y++ {
		for x := 0; x < templateImg.Bounds().Dx(); x++ {
			ti := y*templateImg.Stride + x*4
			if templateImg.Pix[ti+3] == 0 {
				continue
			}
			li := y*live.Stride + x*4
			if templateImg.Pix[ti] != live.Pix[li] ||
				templateImg.Pix[ti+1] != live.Pix[li+1] ||
				templateImg.Pix[ti+2] != live.Pix[li+2] {
				mask.SetNRGBA(x, y, diffColor)
				diff++
			}
		}
	}
	return diff, mask
}

func fillOpaqueMask(mask *image.NRGBA, templateImg *image.NRGBA) {
	diffColor := color.NRGBA{R: 255, G: 0, B: 0, A: 255}
	for y := 0; y < templateImg.Bounds().Dy(); y++ {
		for x := 0; x < templateImg.Bounds().Dx(); x++ {
			idx := y*templateImg.Stride + x*4 + 3
			if templateImg.Pix[idx] != 0 {
				mask.SetNRGBA(x, y, diffColor)
			}
		}
	}
}

func countOpaque(templateImg *image.NRGBA) int {
	count := 0
	for y := 0; y < templateImg.Bounds().Dy(); y++ {
		for x := 0; x < templateImg.Bounds().Dx(); x++ {
			idx := y*templateImg.Stride + x*4 + 3
			if templateImg.Pix[idx] != 0 {
				count++
			}
		}
	}
	return count
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildCombinedPreview(livePNG, diffPNG []byte) ([]byte, error) {
	mergedReader, err := embeds.CombineImages(livePNG, diffPNG)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(mergedReader)
}

func toNRGBAImage(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			dst.Set(x, y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
