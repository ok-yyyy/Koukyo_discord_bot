package notifications

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/utils"
	"Koukyo_discord_bot/internal/wplace"
)

type commonTargetConfig struct {
	ID       string
	Label    string
	Origin   string
	Template string
	Aliases  []string
	Interval time.Duration
}

type watchTemplate struct {
	Img         *image.NRGBA
	Width       int
	Height      int
	OpaqueCount int
}

type watchTemplateCacheEntry struct {
	Template *watchTemplate
	ModTime  time.Time
}

type targetResult struct {
	coord           *utils.Coordinate
	template        *watchTemplate
	diffPixels      int
	diffPercent     float64
	progressPercent float64
	wplaceURL       string
	fullsize        string
	livePNG         []byte
	diffPNG         []byte
	mergedPNG       []byte
}

type rawTarget struct {
	ID              string   `json:"id"`
	Label           string   `json:"label"`
	Origin          string   `json:"origin"`
	Template        string   `json:"template"`
	TemplatePath    string   `json:"template_path"`
	Aliases         []string `json:"aliases"`
	IntervalSeconds int      `json:"interval_seconds"`
	Interval        int      `json:"interval"`
}

func normalizeTargetKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func cleanAliases(aliases []string, id string) []string {
	if len(aliases) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(aliases))
	idKey := normalizeTargetKey(id)
	out := make([]string, 0, len(aliases))
	for _, raw := range aliases {
		a := strings.TrimSpace(raw)
		if a == "" {
			continue
		}
		key := normalizeTargetKey(a)
		if key == "" || key == idKey {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	return out
}

func targetIDMatches(cfg commonTargetConfig, query string) bool {
	q := normalizeTargetKey(query)
	if q == "" {
		return false
	}
	if normalizeTargetKey(cfg.ID) == q {
		return true
	}
	for _, a := range cfg.Aliases {
		if normalizeTargetKey(a) == q {
			return true
		}
	}
	return false
}

func formatManualCommands(id string, aliases []string) string {
	base := fmt.Sprintf("`!%s`", id)
	if len(aliases) == 0 {
		return base
	}
	out := make([]string, 0, len(aliases))
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		out = append(out, fmt.Sprintf("`!%s`", a))
	}
	if len(out) == 0 {
		return base
	}
	return base + "\naliases: " + strings.Join(out, ", ")
}

func loadTargetConfigs(path string, defaultInterval time.Duration) ([]commonTargetConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseTargetConfigs(raw, defaultInterval)
}

func parseTargetConfigs(raw []byte, defaultInterval time.Duration) ([]commonTargetConfig, error) {
	build := func(id string, item rawTarget) (commonTargetConfig, error) {
		cfg := commonTargetConfig{
			ID:       strings.TrimSpace(item.ID),
			Label:    strings.TrimSpace(item.Label),
			Origin:   strings.TrimSpace(item.Origin),
			Template: strings.TrimSpace(item.Template),
		}
		if cfg.Template == "" {
			cfg.Template = strings.TrimSpace(item.TemplatePath)
		}
		if cfg.ID == "" {
			cfg.ID = strings.TrimSpace(id)
		}
		if cfg.ID == "" {
			cfg.ID = cfg.Label
		}
		if cfg.ID == "" {
			return commonTargetConfig{}, fmt.Errorf("target id is empty")
		}
		if cfg.Label == "" {
			cfg.Label = cfg.ID
		}
		cfg.Aliases = cleanAliases(item.Aliases, cfg.ID)
		if cfg.Origin == "" || cfg.Template == "" {
			return commonTargetConfig{}, fmt.Errorf("target %s missing origin/template", cfg.ID)
		}
		sec := item.IntervalSeconds
		if sec <= 0 {
			sec = item.Interval
		}
		if sec <= 0 {
			cfg.Interval = defaultInterval
		} else {
			cfg.Interval = time.Duration(sec) * time.Second
		}
		return cfg, nil
	}

	var root struct {
		Targets []rawTarget `json:"targets"`
	}
	if err := json.Unmarshal(raw, &root); err == nil && len(root.Targets) > 0 {
		out := make([]commonTargetConfig, 0, len(root.Targets))
		for i, item := range root.Targets {
			cfg, err := build(strconv.Itoa(i), item)
			if err != nil {
				return nil, err
			}
			out = append(out, cfg)
		}
		return out, nil
	}

	var asMap map[string]rawTarget
	if err := json.Unmarshal(raw, &asMap); err == nil && len(asMap) > 0 {
		out := make([]commonTargetConfig, 0, len(asMap))
		for key, item := range asMap {
			cfg, err := build(key, item)
			if err != nil {
				return nil, err
			}
			out = append(out, cfg)
		}
		return out, nil
	}

	var asList []rawTarget
	if err := json.Unmarshal(raw, &asList); err == nil && len(asList) > 0 {
		out := make([]commonTargetConfig, 0, len(asList))
		for i, item := range asList {
			cfg, err := build(strconv.Itoa(i), item)
			if err != nil {
				return nil, err
			}
			out = append(out, cfg)
		}
		return out, nil
	}
	return nil, fmt.Errorf("targets json format is invalid")
}

// loadTemplateFromDataDir はテンプレートファイルを data/ 直下から読み込みキャッシュする。
// template_img/ サブディレクトリを使わずに直接ファイル名で指定する。
func loadTemplateFromDataDir(mu *sync.Mutex, cache map[string]*watchTemplateCacheEntry, dataDir, filename string) (*watchTemplate, error) {
	fullPath := filepath.Clean(filepath.Join(dataDir, filename))
	info, err := os.Stat(fullPath)
	if err != nil {
		return nil, fmt.Errorf("template not found: %s", filename)
	}
	mu.Lock()
	if entry, ok := cache[fullPath]; ok && entry.ModTime.Equal(info.ModTime()) {
		mu.Unlock()
		return entry.Template, nil
	}
	mu.Unlock()
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("failed to decode template: %s", filename)
	}
	nrgba := toNRGBAImage(img)
	opaque := countOpaque(nrgba)
	if opaque == 0 {
		return nil, fmt.Errorf("template has no opaque pixels: %s", filename)
	}
	t := &watchTemplate{
		Img:         nrgba,
		Width:       nrgba.Bounds().Dx(),
		Height:      nrgba.Bounds().Dy(),
		OpaqueCount: opaque,
	}
	mu.Lock()
	cache[fullPath] = &watchTemplateCacheEntry{Template: t, ModTime: info.ModTime()}
	mu.Unlock()
	return t, nil
}

func loadTemplateCached(mu *sync.Mutex, cache map[string]*watchTemplateCacheEntry, dataDir, templateRef string) (*watchTemplate, error) {
	templatePath, err := resolveTemplatePath(dataDir, templateRef)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(templatePath)
	if err != nil {
		return nil, fmt.Errorf("template not found: %s", templateRef)
	}

	mu.Lock()
	if entry, ok := cache[templatePath]; ok && entry.ModTime.Equal(info.ModTime()) {
		mu.Unlock()
		return entry.Template, nil
	}
	mu.Unlock()

	f, err := os.Open(templatePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("failed to decode template: %s", templateRef)
	}
	nrgba := toNRGBAImage(img)
	opaque := countOpaque(nrgba)
	if opaque == 0 {
		return nil, fmt.Errorf("template has no opaque pixels: %s", templateRef)
	}
	t := &watchTemplate{
		Img:         nrgba,
		Width:       nrgba.Bounds().Dx(),
		Height:      nrgba.Bounds().Dy(),
		OpaqueCount: opaque,
	}
	mu.Lock()
	cache[templatePath] = &watchTemplateCacheEntry{
		Template: t,
		ModTime:  info.ModTime(),
	}
	mu.Unlock()
	return t, nil
}

func buildTargetResult(coord *utils.Coordinate, template *watchTemplate) (*targetResult, error) {
	startTileX := coord.TileX + coord.PixelX/utils.WplaceTileSize
	startTileY := coord.TileY + coord.PixelY/utils.WplaceTileSize
	startPixelX := coord.PixelX % utils.WplaceTileSize
	startPixelY := coord.PixelY % utils.WplaceTileSize
	endPixelX := startPixelX + template.Width
	endPixelY := startPixelY + template.Height
	tilesX := (endPixelX + utils.WplaceTileSize - 1) / utils.WplaceTileSize
	tilesY := (endPixelY + utils.WplaceTileSize - 1) / utils.WplaceTileSize
	if startTileX < 0 || startTileY < 0 || startTileX+tilesX-1 >= utils.WplaceTilesPerEdge || startTileY+tilesY-1 >= utils.WplaceTilesPerEdge {
		return nil, fmt.Errorf("origin out of range: %s", utils.FormatHyphenCoords(coord))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	tilesData, err := wplace.DownloadTilesGridNoCache(ctx, nil, startTileX, startTileY, tilesX, tilesY, 16)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("download failed: %w", err)
	}

	cropRect := image.Rect(startPixelX, startPixelY, startPixelX+template.Width, startPixelY+template.Height)
	liveImg, err := wplace.CombineTilesCroppedImage(tilesData, utils.WplaceTileSize, utils.WplaceTileSize, tilesX, tilesY, cropRect)
	if err != nil {
		return nil, fmt.Errorf("combine failed: %w", err)
	}

	maskedLive := applyTemplateAlphaMask(template.Img, liveImg)
	diffPixels, diffMask := buildDiffMask(template.Img, liveImg)
	diffPercent := 0.0
	progressPercent := 0.0
	if template.OpaqueCount > 0 {
		diffPercent = float64(diffPixels) * 100 / float64(template.OpaqueCount)
		progressPercent = float64(template.OpaqueCount-diffPixels) * 100 / float64(template.OpaqueCount)
	}

	livePNG, err := encodePNG(maskedLive)
	if err != nil {
		return nil, err
	}
	diffPNG, err := encodePNG(diffMask)
	if err != nil {
		return nil, err
	}
	mergedPNG, err := buildCombinedPreview(livePNG, diffPNG)
	if err != nil {
		return nil, err
	}

	center := watchAreaCenter(coord, template.Width, template.Height)
	return &targetResult{
		coord:           coord,
		template:        template,
		diffPixels:      diffPixels,
		diffPercent:     diffPercent,
		progressPercent: progressPercent,
		wplaceURL:       utils.BuildWplaceURL(center.Lng, center.Lat, utils.ZoomFromImageSize(template.Width, template.Height)),
		fullsize:        fmt.Sprintf("%d-%d-%d-%d-%d-%d", coord.TileX, coord.TileY, coord.PixelX, coord.PixelY, template.Width, template.Height),
		livePNG:         livePNG,
		diffPNG:         diffPNG,
		mergedPNG:       mergedPNG,
	}, nil
}

func targetConfigPath(dataDir, name string) string {
	return filepath.Join(dataDir, name)
}
