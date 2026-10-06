package commands

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"github.com/bwmarrin/discordgo"

	"Koukyo_discord_bot/internal/utils"
	"Koukyo_discord_bot/internal/wplace"
)

const (
	regionMapZoom            = 11
	regionMapSimplifiedZoom  = 7
	regionMapTileSize        = 256
	regionMapUserAgent       = "WplaceDiscordBot/2.3.1"
	regionMapMaxTiles        = 32
	regionMapMaxBytes        = 8 * 1024 * 1024
	regionMapPageSize        = 25
	regionMapTitleHeight     = 80
	regionMapTitleHeightSM   = 60
	regionMapFontSize        = 48
	regionMapFontSizeSM      = 18
	regionMapTitleFontSize   = 20
	regionMapTitleFontSizeSM = 16
	regionMapMaxConcurrent   = 32
	regionMapBaseCacheMax    = 8
	regionMapOverlayCacheMax = 8
	regionMapImageCacheMax   = 16
	regionMapImageCacheTTL   = 5 * time.Minute

	regionMapSelectPrefix  = "regionmap_select:"
	regionMapPagePrefix    = "regionmap_page:"
	regionMapConfirmPrefix = "regionmap_confirm:"
)

type RegionMapCommand struct{}

func NewRegionMapCommand() *RegionMapCommand {
	return &RegionMapCommand{}
}

type baseMapKey struct {
	city string
	zoom int
	minX int
	maxX int
	minY int
	maxY int
}

type baseMapCache struct {
	mu    sync.Mutex
	items map[baseMapKey]*image.NRGBA
}

var regionMapBaseCache = baseMapCache{
	items: make(map[baseMapKey]*image.NRGBA),
}

type overlayKey struct {
	city       string
	zoom       int
	minX       int
	maxX       int
	minY       int
	maxY       int
	minRX      int
	minRY      int
	simplified bool
}

type overlayCache struct {
	mu    sync.Mutex
	items map[overlayKey]*image.NRGBA
}

var regionMapOverlayCache = overlayCache{
	items: make(map[overlayKey]*image.NRGBA),
}

type regionMapImageKey struct {
	city      string
	highlight string
	minRX     int
	maxRX     int
	minRY     int
	maxRY     int
}

type regionMapImageEntry struct {
	data      []byte
	mime      string
	filename  string
	expiresAt time.Time
	element   *list.Element
}

type regionMapImageCache struct {
	mu    sync.Mutex
	items map[regionMapImageKey]*regionMapImageEntry
	order *list.List
}

var regionMapImageCacheStore = regionMapImageCache{
	items: make(map[regionMapImageKey]*regionMapImageEntry),
	order: list.New(),
}

func (c *RegionMapCommand) Name() string { return "regionmap" }

func (c *RegionMapCommand) Description() string {
	return "指定地域のRegion配置マップを表示します"
}

func (c *RegionMapCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	_, err := s.ChannelMessageSend(m.ChannelID, "このコマンドはスラッシュコマンドで利用してください。")
	return err
}

func (c *RegionMapCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	query := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "name" {
			query = strings.TrimSpace(opt.StringValue())
		}
	}
	if query == "" {
		return respondGet(s, i, "❌ 地域名を指定してください。例: /regionmap name:Uruma")
	}
	if err := respondDeferred(s, i); err != nil {
		return err
	}

	embed, file, components, err := buildRegionMapMessage(query, 0, "")
	if err != nil {
		return followupMessage(s, i, "❌ "+err.Error())
	}
	return sendRegionMapFollowup(s, i, embed, file, components)
}

func (c *RegionMapCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "name",
				Description: "地域名 (例: Uruma)",
				Required:    true,
			},
		},
	}
}

func HandleRegionMapPagination(s *discordgo.Session, i *discordgo.InteractionCreate) {
	query, page, ok := parseRegionMapCustomID(i.MessageComponentData().CustomID, regionMapPagePrefix)
	if !ok {
		return
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
	go func() {
		embed, file, components, err := buildRegionMapMessage(query, page, "")
		editRegionMapResponse(s, i, embed, file, components, err)
	}()
}

func HandleRegionMapSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	query, page, ok := parseRegionMapCustomID(i.MessageComponentData().CustomID, regionMapSelectPrefix)
	if !ok {
		return
	}
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	highlight := values[0]
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredMessageUpdate,
	})
	go func() {
		embed, file, components, err := buildRegionMapMessage(query, page, highlight)
		editRegionMapResponse(s, i, embed, file, components, err)
	}()
}

func HandleRegionMapConfirm(s *discordgo.Session, i *discordgo.InteractionCreate, limiter *utils.RateLimiter) {
	regionName, ok := parseRegionMapConfirmID(i.MessageComponentData().CustomID)
	if !ok {
		return
	}
	if err := respondDeferred(s, i); err != nil {
		return
	}
	go func() {
		db, err := loadRegionDBCached()
		if err != nil {
			_ = followupMessage(s, i, "❌ Regionデータベースの読み込みに失敗しました")
			return
		}
		reg, ok := findRegionByName(db, regionName)
		if !ok {
			_ = followupMessage(s, i, "❌ Regionが見つかりません。")
			return
		}
		embed, buf, filename, err := buildRegionDetailImage(reg, limiter)
		if err != nil {
			_ = followupMessage(s, i, "❌ 画像生成に失敗しました: "+err.Error())
			return
		}
		_ = sendImageFollowup(s, i, buf.Bytes(), filename, embed)
	}()
}

func buildRegionMapMessage(query string, page int, highlight string) (*discordgo.MessageEmbed, *discordgo.File, []discordgo.MessageComponent, error) {
	db, err := loadRegionDBCached()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("Regionデータベースの読み込みに失敗しました")
	}
	regions := searchRegionsByCity(db, query)
	if len(regions) == 0 {
		return nil, nil, nil, fmt.Errorf("該当Regionが見つかりません。例: Uruma")
	}

	names := sortedRegionNames(regions)
	page = clampPage(page, len(names), regionMapPageSize)

	data, contentType, filename, err := generateRegionMapImageCached(query, regions, highlight)
	if err != nil {
		return nil, nil, nil, err
	}
	file := &discordgo.File{
		Name:        filename,
		ContentType: contentType,
		Reader:      bytes.NewReader(data),
	}

	description := fmt.Sprintf("地域: %s / 件数: %d", query, len(regions))
	if highlight != "" {
		description += fmt.Sprintf("\n選択中: %s", highlight)
	}
	embed := &discordgo.MessageEmbed{
		Title:       fmt.Sprintf("🗾 %s Region Map", query),
		Description: description,
		Color:       0x3498DB,
		Image: &discordgo.MessageEmbedImage{
			URL: "attachment://" + filename,
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("ページ %d / %d | Map data © OpenStreetMap contributors (openstreetmap.fr/hot)", page+1, totalPages(len(names), regionMapPageSize)),
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	components := buildRegionMapComponents(query, names, page, highlight)
	return embed, file, components, nil
}

func buildRegionMapComponents(query string, names []string, page int, highlight string) []discordgo.MessageComponent {
	total := totalPages(len(names), regionMapPageSize)
	start := page * regionMapPageSize
	end := start + regionMapPageSize
	if end > len(names) {
		end = len(names)
	}
	options := make([]discordgo.SelectMenuOption, 0, end-start)
	for i := start; i < end; i++ {
		name := names[i]
		options = append(options, discordgo.SelectMenuOption{
			Label: truncateRegionLabel(name, 100),
			Value: name,
		})
	}
	encoded := encodeRegionMapKey(query)
	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.SelectMenu{
					CustomID:    fmt.Sprintf("%s%s:%d", regionMapSelectPrefix, encoded, page),
					Placeholder: "Regionを選択",
					Options:     options,
				},
			},
		},
	}
	if total > 1 {
		components = append(components, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "前へ",
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("%s%s:%d", regionMapPagePrefix, encoded, page-1),
					Disabled: page <= 0,
				},
				discordgo.Button{
					Label:    "次へ",
					Style:    discordgo.PrimaryButton,
					CustomID: fmt.Sprintf("%s%s:%d", regionMapPagePrefix, encoded, page+1),
					Disabled: page >= total-1,
				},
			},
		})
	}
	if highlight != "" {
		components = append(components, discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "OK（詳細取得）",
					Style:    discordgo.SuccessButton,
					CustomID: fmt.Sprintf("%s%s", regionMapConfirmPrefix, encodeRegionMapConfirm(highlight)),
				},
			},
		})
	}
	return components
}

func sendRegionMapFollowup(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, file *discordgo.File, components []discordgo.MessageComponent) error {
	params := &discordgo.WebhookParams{
		Embeds:     []*discordgo.MessageEmbed{embed},
		Files:      buildOptionalFiles(file),
		Components: components,
	}
	_, err := s.FollowupMessageCreate(i.Interaction, false, params)
	return err
}

func editRegionMapResponse(s *discordgo.Session, i *discordgo.InteractionCreate, embed *discordgo.MessageEmbed, file *discordgo.File, components []discordgo.MessageComponent, buildErr error) {
	if buildErr != nil {
		msg := "❌ エラー: " + buildErr.Error()
		_, _ = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &msg,
		})
		return
	}
	embeds := []*discordgo.MessageEmbed{embed}
	comps := components
	edit := &discordgo.WebhookEdit{
		Embeds:     &embeds,
		Components: &comps,
	}
	if file != nil {
		edit.Files = []*discordgo.File{file}
		attachments := []*discordgo.MessageAttachment{
			{ID: "0", Filename: file.Name},
		}
		edit.Attachments = &attachments
	} else {
		attachments := []*discordgo.MessageAttachment{}
		edit.Attachments = &attachments
	}
	_, _ = s.InteractionResponseEdit(i.Interaction, edit)
}

func parseRegionMapCustomID(customID, prefix string) (string, int, bool) {
	if !strings.HasPrefix(customID, prefix) {
		return "", 0, false
	}
	payload := strings.TrimPrefix(customID, prefix)
	parts := strings.SplitN(payload, ":", 2)
	if len(parts) != 2 {
		return "", 0, false
	}
	query, err := decodeRegionMapKey(parts[0])
	if err != nil {
		return "", 0, false
	}
	page, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", 0, false
	}
	return query, page, true
}

func encodeRegionMapKey(value string) string {
	escaped := url.QueryEscape(strings.TrimSpace(value))
	return base64.RawURLEncoding.EncodeToString([]byte(escaped))
}

func decodeRegionMapKey(value string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", err
	}
	return url.QueryUnescape(string(raw))
}

func encodeRegionMapConfirm(value string) string {
	escaped := url.QueryEscape(strings.TrimSpace(value))
	return base64.RawURLEncoding.EncodeToString([]byte(escaped))
}

func parseRegionMapConfirmID(customID string) (string, bool) {
	if !strings.HasPrefix(customID, regionMapConfirmPrefix) {
		return "", false
	}
	payload := strings.TrimPrefix(customID, regionMapConfirmPrefix)
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", false
	}
	name, err := url.QueryUnescape(string(raw))
	if err != nil {
		return "", false
	}
	if name == "" {
		return "", false
	}
	return name, true
}

func truncateRegionLabel(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}

func totalPages(total, pageSize int) int {
	if total <= 0 {
		return 1
	}
	return (total + pageSize - 1) / pageSize
}

func clampPage(page, total, pageSize int) int {
	maxPage := 0
	if total > 0 {
		maxPage = (total - 1) / pageSize
	}
	if page < 0 {
		return 0
	}
	if page > maxPage {
		return maxPage
	}
	return page
}

func buildRegionDetailImage(reg Region, limiter *utils.RateLimiter) (*discordgo.MessageEmbed, *bytes.Buffer, string, error) {
	minTileX, minTileY := reg.TileRange.Min[0], reg.TileRange.Min[1]
	maxTileX, maxTileY := reg.TileRange.Max[0], reg.TileRange.Max[1]
	if minTileX < 0 || minTileY < 0 || maxTileX >= utils.WplaceTilesPerEdge || maxTileY >= utils.WplaceTilesPerEdge {
		return nil, nil, "", fmt.Errorf("Regionタイル範囲が無効です: X[%d-%d] Y[%d-%d]", minTileX, maxTileX, minTileY, maxTileY)
	}
	gridCols := maxTileX - minTileX + 1
	gridRows := maxTileY - minTileY + 1
	if gridCols <= 0 || gridRows <= 0 {
		return nil, nil, "", fmt.Errorf("Regionタイル範囲が無効です。")
	}
	totalTiles := gridCols * gridRows
	if totalTiles > regionMapMaxTiles {
		return nil, nil, "", fmt.Errorf("Regionが大きすぎます（%dタイル）。最大%dタイルまで詳細取得可能です。", totalTiles, regionMapMaxTiles)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	tilesData, err := wplace.DownloadTilesGrid(ctx, limiter, minTileX, minTileY, gridCols, gridRows, 16)
	cancel()
	if err != nil {
		return nil, nil, "", err
	}

	buf, err := combineTiles(tilesData, utils.WplaceTileSize, utils.WplaceTileSize, gridCols, gridRows)
	if err != nil {
		return nil, nil, "", err
	}

	displayName := fmt.Sprintf("%s_%d", reg.Name, reg.CountryID)
	filename := fmt.Sprintf("%s_full.png", strings.ReplaceAll(displayName, "#", "_"))
	centerLat := reg.CenterLatLng[0]
	centerLng := reg.CenterLatLng[1]
	imageWidth := gridCols * utils.WplaceTileSize
	imageHeight := gridRows * utils.WplaceTileSize
	wplaceURL := utils.BuildWplaceURL(centerLng, centerLat, calculateZoomFromWH(imageWidth, imageHeight))

	embed := &discordgo.MessageEmbed{
		Title: fmt.Sprintf("🗺️ %s 全域画像", displayName),
		Color: 0x5865F2,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "Region ID",
				Value:  fmt.Sprintf("`%d`", reg.RegionID),
				Inline: true,
			},
			{
				Name:   "City ID",
				Value:  fmt.Sprintf("`%d`", reg.CityID),
				Inline: true,
			},
			{
				Name:   "タイル範囲",
				Value:  fmt.Sprintf("X[%d-%d] Y[%d-%d]", minTileX, maxTileX, minTileY, maxTileY),
				Inline: false,
			},
			{
				Name:   "画像サイズ",
				Value:  fmt.Sprintf("%dx%dpx", imageWidth, imageHeight),
				Inline: true,
			},
			{
				Name:   "タイル数",
				Value:  fmt.Sprintf("%dタイル (%d×%d)", totalTiles, gridCols, gridRows),
				Inline: true,
			},
			{
				Name:   "中心座標",
				Value:  fmt.Sprintf("緯度: %.4f, 経度: %.4f", centerLat, centerLng),
				Inline: false,
			},
			{
				Name:   "Wplace.live",
				Value:  fmt.Sprintf("[地図で見る](%s)", wplaceURL),
				Inline: false,
			},
		},
		Image: &discordgo.MessageEmbedImage{
			URL: "attachment://" + filename,
		},
	}
	return embed, buf, filename, nil
}

func searchRegionsByCity(database RegionDB, cityName string) map[string]Region {
	results := make(map[string]Region)
	query := strings.ToLower(strings.TrimSpace(cityName))
	if query == "" {
		return results
	}
	for regionName, info := range database {
		lower := strings.ToLower(regionName)
		if strings.HasPrefix(lower, query+"#") {
			results[regionName] = info
		}
	}
	return results
}

func sortedRegionNames(regions map[string]Region) []string {
	names := make([]string, 0, len(regions))
	for name := range regions {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		ni, okI := parseRegionNumber(names[i])
		nj, okJ := parseRegionNumber(names[j])
		if okI && okJ && ni != nj {
			return ni < nj
		}
		return names[i] < names[j]
	})
	return names
}

func parseRegionNumber(name string) (int, bool) {
	parts := strings.SplitN(name, "#", 2)
	if len(parts) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

func generateRegionMapImageCached(cityName string, regions map[string]Region, highlight string) ([]byte, string, string, error) {
	minRX, maxRX, minRY, maxRY, ok := calculateRegionBounds(regions)
	if !ok {
		return nil, "", "", fmt.Errorf("Region座標が不正です")
	}
	key := regionMapImageKey{
		city:      cityNameLower(cityName),
		highlight: strings.ToLower(strings.TrimSpace(highlight)),
		minRX:     minRX,
		maxRX:     maxRX,
		minRY:     minRY,
		maxRY:     maxRY,
	}
	if data, mime, filename, ok := getRegionMapImageCache(key); ok {
		return data, mime, filename, nil
	}
	data, contentType, filename, err := generateSimplifiedRegionMap(cityName, regions, highlight)
	if err != nil {
		return nil, "", "", err
	}
	storeRegionMapImageCache(key, data, contentType, filename)
	return data, contentType, filename, nil
}

func generateSimplifiedRegionMap(cityName string, regions map[string]Region, highlight string) ([]byte, string, string, error) {
	minRX, maxRX, minRY, maxRY, ok := calculateRegionBounds(regions)
	if !ok {
		return nil, "", "", fmt.Errorf("Region座標が不正です")
	}

	scale := 4
	minTX := minRX / scale
	maxTX := maxRX / scale
	minTY := minRY / scale
	maxTY := maxRY / scale

	tilesX := maxTX - minTX + 1
	tilesY := maxTY - minTY + 1

	mapWidth := tilesX * regionMapTileSize
	mapHeight := tilesY * regionMapTileSize
	key := baseMapKey{
		city: strings.ToLower(cityName),
		zoom: regionMapSimplifiedZoom,
		minX: minTX,
		maxX: maxTX,
		minY: minTY,
		maxY: maxTY,
	}
	baseMap := getBaseMapCached(key)
	if baseMap == nil {
		baseMap = buildBaseMap(mapWidth, mapHeight, minTX, maxTX, minTY, maxTY, regionMapSimplifiedZoom)
		storeBaseMapCached(key, baseMap)
	}
	baseMap = cloneNRGBA(baseMap)

	overlayKey := overlayKey{
		city:       cityNameLower(cityName),
		zoom:       regionMapSimplifiedZoom,
		minX:       minTX,
		maxX:       maxTX,
		minY:       minTY,
		maxY:       maxTY,
		minRX:      minRX,
		minRY:      minRY,
		simplified: true,
	}
	overlay := getOverlayCached(overlayKey)
	if overlay == nil {
		overlay = buildSimplifiedOverlay(mapWidth, mapHeight, minRX, minRY, regions)
		storeOverlayCached(overlayKey, overlay)
	}
	draw.Draw(baseMap, baseMap.Bounds(), overlay, image.Point{}, draw.Over)

	cellSize := regionMapTileSize / scale
	for regionName, info := range regions {
		rx, ry := info.RegionCoords[0], info.RegionCoords[1]
		col := rx - minRX
		row := ry - minRY
		x1 := col * cellSize
		y1 := row * cellSize
		x2 := x1 + cellSize
		y2 := y1 + cellSize

		if regionName == highlight {
			highlightOverlay := color.NRGBA{255, 215, 0, 100}
			highlightBorder := color.NRGBA{255, 140, 0, 200}
			fillRect(baseMap, x1, y1, x2, y2, highlightOverlay)
			strokeRect(baseMap, x1, y1, x2, y2, highlightBorder, 2)
			numText := regionLabel(regionName, info)
			drawCenteredText(baseMap, numText, x1, y1, x2, y2, true, regionMapFontSizeSM)
		}
	}

	titleHeight := regionMapTitleHeightSM
	finalImage := image.NewNRGBA(image.Rect(0, 0, mapWidth, mapHeight+titleHeight))
	draw.Draw(finalImage, finalImage.Bounds(), &image.Uniform{C: color.NRGBA{0x2C, 0x3E, 0x50, 0xFF}}, image.Point{}, draw.Src)
	draw.Draw(finalImage, image.Rect(0, titleHeight, mapWidth, mapHeight+titleHeight), baseMap, image.Point{}, draw.Src)
	title := fmt.Sprintf("🗾 %s Region Map (%d regions) - Simplified", cityName, len(regions))
	drawTitle(finalImage, title, mapWidth, titleHeight, regionMapTitleFontSizeSM)

	data, contentType, filename, err := encodeRegionMap(finalImage)
	if err != nil {
		return nil, "", "", err
	}
	return data, contentType, filename, nil
}

func calculateRegionBounds(regions map[string]Region) (int, int, int, int, bool) {
	if len(regions) == 0 {
		return 0, 0, 0, 0, false
	}
	first := true
	var minRX, maxRX, minRY, maxRY int
	for _, info := range regions {
		rx := info.RegionCoords[0]
		ry := info.RegionCoords[1]
		if first {
			minRX, maxRX, minRY, maxRY = rx, rx, ry, ry
			first = false
			continue
		}
		if rx < minRX {
			minRX = rx
		}
		if rx > maxRX {
			maxRX = rx
		}
		if ry < minRY {
			minRY = ry
		}
		if ry > maxRY {
			maxRY = ry
		}
	}
	return minRX, maxRX, minRY, maxRY, true
}

func regionLabel(name string, info Region) string {
	if n, ok := parseRegionNumber(name); ok {
		return fmt.Sprintf("#%d", n)
	}
	if n, ok := parseRegionNumber(info.Name); ok {
		return fmt.Sprintf("#%d", n)
	}
	return "#?"
}

func fillRect(img draw.Image, x1, y1, x2, y2 int, c color.NRGBA) {
	rect := image.Rect(x1, y1, x2, y2)
	draw.Draw(img, rect, &image.Uniform{C: c}, image.Point{}, draw.Over)
}

func strokeRect(img draw.Image, x1, y1, x2, y2 int, c color.NRGBA, width int) {
	if width <= 0 {
		width = 1
	}
	for i := 0; i < width; i++ {
		top := image.Rect(x1+i, y1+i, x2-i, y1+i+1)
		bottom := image.Rect(x1+i, y2-i-1, x2-i, y2-i)
		left := image.Rect(x1+i, y1+i, x1+i+1, y2-i)
		right := image.Rect(x2-i-1, y1+i, x2-i, y2-i)
		draw.Draw(img, top, &image.Uniform{C: c}, image.Point{}, draw.Over)
		draw.Draw(img, bottom, &image.Uniform{C: c}, image.Point{}, draw.Over)
		draw.Draw(img, left, &image.Uniform{C: c}, image.Point{}, draw.Over)
		draw.Draw(img, right, &image.Uniform{C: c}, image.Point{}, draw.Over)
	}
}

func drawCenteredText(img draw.Image, text string, x1, y1, x2, y2 int, highlight bool, size float64) {
	face := resolveFontFace(size, basicfont.Face7x13)
	textWidth := font.MeasureString(face, text).Ceil()
	textHeight := face.Metrics().Height.Ceil()
	ascent := face.Metrics().Ascent.Ceil()

	textX := x1 + (x2-x1-textWidth)/2
	textY := y1 + (y2-y1-textHeight)/2 + ascent

	outline := color.NRGBA{255, 255, 255, 255}
	for _, dx := range []int{-2, 0, 2} {
		for _, dy := range []int{-2, 0, 2} {
			if dx == 0 && dy == 0 {
				continue
			}
			drawText(img, text, textX+dx, textY+dy, outline, face)
		}
	}
	textColor := color.NRGBA{0, 0, 139, 255}
	if highlight {
		textColor = color.NRGBA{255, 140, 0, 255}
	}
	drawText(img, text, textX, textY, textColor, face)
}

func drawTitle(img draw.Image, text string, width, height int, size float64) {
	face := resolveFontFace(size, basicfont.Face7x13)
	textWidth := font.MeasureString(face, text).Ceil()
	ascent := face.Metrics().Ascent.Ceil()
	x := (width - textWidth) / 2
	y := (height-ascent)/2 + ascent
	drawText(img, text, x, y, color.NRGBA{255, 255, 255, 255}, face)
}

func drawText(img draw.Image, text string, x, y int, c color.NRGBA, face font.Face) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(text)
}

func resolveFontFace(size float64, fallback font.Face) font.Face {
	face := getGoFontFace(size)
	if face != nil {
		return face
	}
	return fallback
}

var (
	gofontOnce      sync.Once
	gofontFaceCache = make(map[float64]font.Face)
	gofontErr       error
	gofontMu        sync.Mutex
	gofontData      *opentype.Font
)

func getGoFontFace(size float64) font.Face {
	gofontOnce.Do(func() {
		gofontData, gofontErr = opentype.Parse(goregular.TTF)
	})
	if gofontErr != nil || gofontData == nil {
		return nil
	}
	gofontMu.Lock()
	defer gofontMu.Unlock()
	if face, ok := gofontFaceCache[size]; ok {
		return face
	}
	face, err := opentype.NewFace(gofontData, &opentype.FaceOptions{
		Size:    size,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil
	}
	gofontFaceCache[size] = face
	return face
}

func encodeRegionMap(img image.Image) ([]byte, string, string, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err == nil {
		if buf.Len() <= regionMapMaxBytes {
			return buf.Bytes(), "image/png", "region_map.png", nil
		}
	}
	buf.Reset()
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		return nil, "", "", err
	}
	return buf.Bytes(), "image/jpeg", "region_map.jpg", nil
}

func buildBaseMap(mapWidth, mapHeight, minTX, maxTX, minTY, maxTY, zoom int) *image.NRGBA {
	baseMap := image.NewNRGBA(image.Rect(0, 0, mapWidth, mapHeight))
	draw.Draw(baseMap, baseMap.Bounds(), &image.Uniform{C: color.NRGBA{0xE8, 0xE8, 0xE8, 0xFF}}, image.Point{}, draw.Src)
	tiles := regionMapGenerator.fetchTiles(minTX, maxTX, minTY, maxTY, zoom)
	for key, tile := range tiles {
		if tile == nil {
			continue
		}
		xOffset := (key.x - minTX) * regionMapTileSize
		yOffset := (key.y - minTY) * regionMapTileSize
		draw.Draw(baseMap, image.Rect(xOffset, yOffset, xOffset+regionMapTileSize, yOffset+regionMapTileSize), tile, image.Point{}, draw.Src)
	}
	return baseMap
}

func buildSimplifiedOverlay(mapWidth, mapHeight, minRX, minRY int, regions map[string]Region) *image.NRGBA {
	overlay := image.NewNRGBA(image.Rect(0, 0, mapWidth, mapHeight))
	cellSize := regionMapTileSize / 4
	for regionName, info := range regions {
		rx, ry := info.RegionCoords[0], info.RegionCoords[1]
		col := rx - minRX
		row := ry - minRY
		x1 := col * cellSize
		y1 := row * cellSize
		x2 := x1 + cellSize
		y2 := y1 + cellSize
		fillRect(overlay, x1, y1, x2, y2, color.NRGBA{100, 149, 237, 50})
		strokeRect(overlay, x1, y1, x2, y2, color.NRGBA{70, 130, 220, 150}, 2)
		numText := regionLabel(regionName, info)
		drawCenteredText(overlay, numText, x1, y1, x2, y2, false, regionMapFontSizeSM)
	}
	return overlay
}

func cityNameLower(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func getOverlayCached(key overlayKey) *image.NRGBA {
	regionMapOverlayCache.mu.Lock()
	defer regionMapOverlayCache.mu.Unlock()
	if img, ok := regionMapOverlayCache.items[key]; ok {
		return img
	}
	return nil
}

func storeOverlayCached(key overlayKey, img *image.NRGBA) {
	regionMapOverlayCache.mu.Lock()
	defer regionMapOverlayCache.mu.Unlock()
	if len(regionMapOverlayCache.items) >= regionMapOverlayCacheMax {
		regionMapOverlayCache.items = make(map[overlayKey]*image.NRGBA)
	}
	regionMapOverlayCache.items[key] = img
}

func getRegionMapImageCache(key regionMapImageKey) ([]byte, string, string, bool) {
	regionMapImageCacheStore.mu.Lock()
	defer regionMapImageCacheStore.mu.Unlock()
	entry, ok := regionMapImageCacheStore.items[key]
	if !ok {
		return nil, "", "", false
	}
	if time.Now().After(entry.expiresAt) {
		removeRegionMapImageCacheEntryLocked(key, entry)
		return nil, "", "", false
	}
	if entry.element != nil {
		regionMapImageCacheStore.order.MoveToFront(entry.element)
	}
	return entry.data, entry.mime, entry.filename, true
}

func storeRegionMapImageCache(key regionMapImageKey, data []byte, mime, filename string) {
	regionMapImageCacheStore.mu.Lock()
	defer regionMapImageCacheStore.mu.Unlock()
	if existing, ok := regionMapImageCacheStore.items[key]; ok {
		existing.data = data
		existing.mime = mime
		existing.filename = filename
		existing.expiresAt = time.Now().Add(regionMapImageCacheTTL)
		if existing.element != nil {
			regionMapImageCacheStore.order.MoveToFront(existing.element)
		}
		return
	}
	entry := &regionMapImageEntry{
		data:      data,
		mime:      mime,
		filename:  filename,
		expiresAt: time.Now().Add(regionMapImageCacheTTL),
	}
	entry.element = regionMapImageCacheStore.order.PushFront(key)
	regionMapImageCacheStore.items[key] = entry
	for regionMapImageCacheStore.order.Len() > regionMapImageCacheMax {
		back := regionMapImageCacheStore.order.Back()
		if back == nil {
			break
		}
		backKey, ok := back.Value.(regionMapImageKey)
		if !ok {
			regionMapImageCacheStore.order.Remove(back)
			continue
		}
		if victim, ok := regionMapImageCacheStore.items[backKey]; ok {
			removeRegionMapImageCacheEntryLocked(backKey, victim)
		} else {
			regionMapImageCacheStore.order.Remove(back)
		}
	}
}

func removeRegionMapImageCacheEntryLocked(key regionMapImageKey, entry *regionMapImageEntry) {
	delete(regionMapImageCacheStore.items, key)
	if entry.element != nil {
		regionMapImageCacheStore.order.Remove(entry.element)
	}
}

func getBaseMapCached(key baseMapKey) *image.NRGBA {
	regionMapBaseCache.mu.Lock()
	defer regionMapBaseCache.mu.Unlock()
	if img, ok := regionMapBaseCache.items[key]; ok {
		return img
	}
	return nil
}

func storeBaseMapCached(key baseMapKey, img *image.NRGBA) {
	regionMapBaseCache.mu.Lock()
	defer regionMapBaseCache.mu.Unlock()
	if len(regionMapBaseCache.items) >= regionMapBaseCacheMax {
		regionMapBaseCache.items = make(map[baseMapKey]*image.NRGBA)
	}
	regionMapBaseCache.items[key] = img
}

func toNRGBA(src image.Image) *image.NRGBA {
	b := src.Bounds()
	dst := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(dst, dst.Bounds(), src, b.Min, draw.Src)
	return dst
}

func cloneNRGBA(src *image.NRGBA) *image.NRGBA {
	if src == nil {
		return nil
	}
	dst := image.NewNRGBA(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}

type tileKey struct {
	x int
	y int
}

type RegionMapGenerator struct {
	client    *http.Client
	userAgent string
	cache     map[string]*image.NRGBA
	mu        sync.Mutex
	rr        int
}

var regionMapGenerator = newRegionMapGenerator()

func newRegionMapGenerator() *RegionMapGenerator {
	transport := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		MaxConnsPerHost:       32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &RegionMapGenerator{
		client: &http.Client{
			Timeout:   12 * time.Second,
			Transport: transport,
		},
		userAgent: regionMapUserAgent,
		cache:     make(map[string]*image.NRGBA),
	}
}

func (g *RegionMapGenerator) fetchTiles(minTX, maxTX, minTY, maxTY, zoom int) map[tileKey]*image.NRGBA {
	result := make(map[tileKey]*image.NRGBA)
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, regionMapMaxConcurrent)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	for ty := minTY; ty <= maxTY; ty++ {
		for tx := minTX; tx <= maxTX; tx++ {
			wg.Add(1)
			sem <- struct{}{}
			go func(x, y int) {
				defer wg.Done()
				defer func() { <-sem }()
				tile, _ := g.getTile(ctx, zoom, x, y)
				mu.Lock()
				result[tileKey{x: x, y: y}] = tile
				mu.Unlock()
			}(tx, ty)
		}
	}
	wg.Wait()
	return result
}

func (g *RegionMapGenerator) getTile(ctx context.Context, zoom, x, y int) (*image.NRGBA, error) {
	cacheKey := fmt.Sprintf("%d/%d/%d", zoom, x, y)
	g.mu.Lock()
	if img, ok := g.cache[cacheKey]; ok {
		g.mu.Unlock()
		return img, nil
	}
	g.mu.Unlock()

	subdomain := g.nextSubdomain()
	tileURL := fmt.Sprintf("https://%s.tile.openstreetmap.fr/hot/%d/%d/%d.png", subdomain, zoom, x, y)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tileURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", g.userAgent)

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tile fetch failed: %s", resp.Status)
	}
	img, err := png.Decode(resp.Body)
	if err != nil {
		return nil, err
	}
	tile := toNRGBA(img)

	g.mu.Lock()
	g.cache[cacheKey] = tile
	g.mu.Unlock()
	return tile, nil
}

func (g *RegionMapGenerator) nextSubdomain() string {
	subdomains := []string{"a", "b", "c"}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rr++
	return subdomains[g.rr%len(subdomains)]
}
