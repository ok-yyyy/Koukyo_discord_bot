package wplace

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/utils"
)

const tileCacheTTL = 2 * time.Minute

var tileHTTPClient = &http.Client{
	Timeout: 12 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

type tileCacheEntry struct {
	data      []byte
	expiresAt time.Time
}

var tileCache struct {
	mu    sync.RWMutex
	items map[string]tileCacheEntry
}

var (
	tileURLFormat string
	urlFormatMu   sync.RWMutex
)

func init() {
	tileCache.items = make(map[string]tileCacheEntry)
	detectTileURLFormat()
}

func detectTileURLFormat() {
	// Test both URL formats with a known tile (0, 0)
	formats := []string{
		"https://backend.wplace.live/tile/%d/%d.png",
		"https://backend.wplace.live/files/s0/tiles/%d/%d.png",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for _, format := range formats {
		testURL := fmt.Sprintf(format, 0, 0)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
		if err != nil {
			continue
		}

		resp, err := tileHTTPClient.Do(req)
		if err != nil {
			log.Printf("Tile URL format test failed for %s: %v", format, err)
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			tileURLFormat = format
			log.Printf("✅ Detected working tile URL format: %s", format)
			return
		}
		log.Printf("Tile URL format %s returned status %d", format, resp.StatusCode)
	}

	// Fallback to newer format
	tileURLFormat = formats[0]
	log.Printf("⚠️ No working tile URL format detected, using default: %s", tileURLFormat)
}

func DownloadTile(ctx context.Context, limiter *utils.RateLimiter, tileX, tileY int) ([]byte, error) {
	return downloadTile(ctx, limiter, tileX, tileY, true)
}

func DownloadTileNoCache(ctx context.Context, limiter *utils.RateLimiter, tileX, tileY int) ([]byte, error) {
	return downloadTile(ctx, limiter, tileX, tileY, false)
}

func downloadTile(ctx context.Context, limiter *utils.RateLimiter, tileX, tileY int, useCache bool) ([]byte, error) {
	cacheBust := time.Now().UnixNano() % 10000000
	urlFormatMu.RLock()
	format := tileURLFormat
	urlFormatMu.RUnlock()
	url := fmt.Sprintf(format+"?t=%d", tileX, tileY, cacheBust)
	cacheKey := fmt.Sprintf("%d-%d", tileX, tileY)
	if useCache {
		if data, ok := getTileFromCache(cacheKey); ok {
			return data, nil
		}
	}

	doReq := func() (interface{}, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := tileHTTPClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("HTTP GET failed for %s: %w", url, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("failed to download tile %d-%d (URL: %s), status: %s", tileX, tileY, url, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}

	var (
		val interface{}
		err error
	)
	if limiter != nil {
		val, err = limiter.Do(ctx, "backend.wplace.live", doReq)
	} else {
		val, err = doReq()
	}
	if err != nil {
		return nil, err
	}
	data, ok := val.([]byte)
	if !ok {
		return nil, fmt.Errorf("unexpected response type for tile %d-%d", tileX, tileY)
	}
	if useCache && len(data) > 0 {
		storeTileCache(cacheKey, data)
	}
	return data, nil
}

func DownloadTilesGrid(
	ctx context.Context,
	limiter *utils.RateLimiter,
	minX, minY, cols, rows, maxConcurrent int,
) ([][]byte, error) {
	return downloadTilesGrid(ctx, limiter, minX, minY, cols, rows, maxConcurrent, true)
}

func DownloadTilesGridNoCache(
	ctx context.Context,
	limiter *utils.RateLimiter,
	minX, minY, cols, rows, maxConcurrent int,
) ([][]byte, error) {
	return downloadTilesGrid(ctx, limiter, minX, minY, cols, rows, maxConcurrent, false)
}

func downloadTilesGrid(
	ctx context.Context,
	limiter *utils.RateLimiter,
	minX, minY, cols, rows, maxConcurrent int,
	useCache bool,
) ([][]byte, error) {
	if cols <= 0 || rows <= 0 {
		return nil, fmt.Errorf("invalid grid size: %dx%d", cols, rows)
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 1
	}

	total := cols * rows
	tiles := make([][]byte, total)
	var mu sync.Mutex
	var firstErr error
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	type tileJob struct {
		tileX int
		tileY int
		index int
	}

	workers := maxConcurrent
	if workers > total {
		workers = total
	}
	if workers <= 0 {
		workers = 1
	}

	jobs := make(chan tileJob, workers)
	var workerWG sync.WaitGroup

	setFirstErr := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}

	for i := 0; i < workers; i++ {
		workerWG.Add(1)
		go func() {
			defer workerWG.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}

				reqCtx, cancelReq := context.WithTimeout(ctx, 15*time.Second)
				var data []byte
				var err error
				if useCache {
					data, err = DownloadTile(reqCtx, limiter, job.tileX, job.tileY)
				} else {
					data, err = DownloadTileNoCache(reqCtx, limiter, job.tileX, job.tileY)
				}
				cancelReq()
				if err != nil {
					setFirstErr(err)
					return
				}
				tiles[job.index] = data
			}
		}()
	}

enqueueLoop:
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			select {
			case <-ctx.Done():
				break enqueueLoop
			case jobs <- tileJob{
				tileX: minX + x,
				tileY: minY + y,
				index: y*cols + x,
			}:
			}
		}
	}
	close(jobs)
	workerWG.Wait()

	if firstErr != nil {
		return nil, firstErr
	}
	for i, data := range tiles {
		if len(data) == 0 {
			return nil, fmt.Errorf("tile download failed (index=%d)", i)
		}
	}
	return tiles, nil
}

func CombineTilesImage(tilesData [][]byte, tileWidth, tileHeight, gridCols, gridRows int) (*image.NRGBA, error) {
	if len(tilesData) == 0 {
		return nil, fmt.Errorf("no tile data")
	}
	if len(tilesData) != gridCols*gridRows {
		return nil, fmt.Errorf("tile data count mismatch: %d != %d", len(tilesData), gridCols*gridRows)
	}
	out := image.NewNRGBA(image.Rect(0, 0, tileWidth*gridCols, tileHeight*gridRows))
	for i, data := range tilesData {
		tile, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to decode tile index %d: %w", i, err)
		}
		col := i % gridCols
		row := i / gridCols
		dp := image.Pt(col*tileWidth, row*tileHeight)
		draw.Draw(out, tile.Bounds().Add(dp), tile, tile.Bounds().Min, draw.Src)
	}
	return out, nil
}

func CombineTilesCroppedImage(
	tilesData [][]byte,
	tileWidth, tileHeight, gridCols, gridRows int,
	cropRect image.Rectangle,
) (*image.NRGBA, error) {
	if len(tilesData) == 0 {
		return nil, fmt.Errorf("no tile data")
	}
	if len(tilesData) != gridCols*gridRows {
		return nil, fmt.Errorf("tile data count mismatch: %d != %d", len(tilesData), gridCols*gridRows)
	}
	if cropRect.Dx() <= 0 || cropRect.Dy() <= 0 {
		return nil, fmt.Errorf("invalid crop rectangle")
	}

	out := image.NewNRGBA(image.Rect(0, 0, cropRect.Dx(), cropRect.Dy()))
	for i, data := range tilesData {
		col := i % gridCols
		row := i / gridCols
		tileRect := image.Rect(col*tileWidth, row*tileHeight, (col+1)*tileWidth, (row+1)*tileHeight)
		inter := tileRect.Intersect(cropRect)
		if inter.Dx() <= 0 || inter.Dy() <= 0 {
			continue
		}
		tile, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("failed to decode tile index %d: %w", i, err)
		}
		dstRect := image.Rect(
			inter.Min.X-cropRect.Min.X,
			inter.Min.Y-cropRect.Min.Y,
			inter.Max.X-cropRect.Min.X,
			inter.Max.Y-cropRect.Min.Y,
		)
		srcPt := image.Pt(inter.Min.X-tileRect.Min.X, inter.Min.Y-tileRect.Min.Y)
		draw.Draw(out, dstRect, tile, srcPt, draw.Src)
	}
	return out, nil
}

func getTileFromCache(key string) ([]byte, bool) {
	now := time.Now()

	tileCache.mu.RLock()
	entry, ok := tileCache.items[key]
	tileCache.mu.RUnlock()
	if !ok {
		return nil, false
	}

	if now.After(entry.expiresAt) {
		tileCache.mu.Lock()
		latest, ok := tileCache.items[key]
		if ok && now.After(latest.expiresAt) {
			delete(tileCache.items, key)
		}
		tileCache.mu.Unlock()
		return nil, false
	}
	return entry.data, true
}

func storeTileCache(key string, data []byte) {
	tileCache.mu.Lock()
	defer tileCache.mu.Unlock()
	tileCache.items[key] = tileCacheEntry{
		data:      data,
		expiresAt: time.Now().Add(tileCacheTTL),
	}
}
