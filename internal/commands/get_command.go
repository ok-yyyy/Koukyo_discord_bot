package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"Koukyo_discord_bot/internal/utils"

	"github.com/bwmarrin/discordgo"
)

type GetCommand struct {
	limiter *utils.RateLimiter
}

func NewGetCommand(limiter *utils.RateLimiter) *GetCommand {
	return &GetCommand{limiter: limiter}
}

func (c *GetCommand) Name() string {
	return "get"
}

func (c *GetCommand) Description() string {
	return "画像やデータを取得します。"
}

func (c *GetCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	_, err := s.ChannelMessageSend(m.ChannelID, "このコマンドはスラッシュコマンドで利用してください。")
	return err
}

const maxTilesLimit = 16

func (c *GetCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	options := i.ApplicationCommandData().Options
	var (
		coords   string
		region   string
		fullsize string
	)
	for _, opt := range options {
		switch opt.Name {
		case "coords":
			coords = opt.StringValue()
		case "region":
			region = opt.StringValue()
		case "fullsize":
			fullsize = opt.StringValue()
		}
	}

	if coords != "" {
		parts := strings.Split(coords, "-")
		if len(parts) != 2 {
			return respondGet(s, i, "❌ 座標形式が正しくありません: TlX-TlY 例: 1818-806")
		}
		tileX, errX := strconv.Atoi(parts[0])
		tileY, errY := strconv.Atoi(parts[1])
		if errX != nil || errY != nil {
			return respondGet(s, i, "❌ 座標値が不正です。整数で指定してください。")
		}
		if tileX < 0 || tileX >= 2048 || tileY < 0 || tileY >= 2048 {
			return respondGet(s, i, fmt.Sprintf("❌ タイル座標が範囲外です: %d-%d 有効範囲: 0～2047", tileX, tileY))
		}

		if err := respondDeferred(s, i); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		imageData, err := c.downloadTile(ctx, tileX, tileY)
		cancel()
		if err != nil {
			return followupMessage(s, i, fmt.Sprintf("❌ タイル画像のダウンロードに失敗しました: %v", err))
		}
		latLng := utils.TilePixelCenterToLngLat(tileX, tileY, utils.WplaceTileSize/2, utils.WplaceTileSize/2)
		wplaceURL := utils.BuildWplaceURL(latLng.Lng, latLng.Lat, calculateZoomFromWH(utils.WplaceTileSize, utils.WplaceTileSize))
		filename := fmt.Sprintf("tile_%d-%d.png", tileX, tileY)
		embed := &discordgo.MessageEmbed{
			Title:       fmt.Sprintf("🗺️ タイル画像: %d-%d", tileX, tileY),
			Description: fmt.Sprintf("[Wplaceで開く](%s)", wplaceURL),
			Color:       0x5865F2,
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "タイル座標",
					Value:  fmt.Sprintf("`%d-%d`", tileX, tileY),
					Inline: true,
				},
				{
					Name:   "中心座標",
					Value:  fmt.Sprintf("`%.6f, %.6f`", latLng.Lng, latLng.Lat),
					Inline: true,
				},
			},
			Image: &discordgo.MessageEmbedImage{
				URL: "attachment://" + filename,
			},
		}
		return sendImageFollowup(s, i, imageData, filename, embed)
	}

	if region != "" {
		if err := respondDeferred(s, i); err != nil {
			return err
		}
		db, err := loadRegionDB()
		if err != nil {
			return followupMessage(s, i, "Regionデータベースの読み込みに失敗しました。")
		}
		reg, ok := findRegionByName(db, region)
		if !ok {
			return followupMessage(s, i, "❌ Regionが見つかりません。例: Tokyo#1, Osaka#1 など")
		}

		minTileX, minTileY := reg.TileRange.Min[0], reg.TileRange.Min[1]
		maxTileX, maxTileY := reg.TileRange.Max[0], reg.TileRange.Max[1]
		if minTileX < 0 || minTileY < 0 || maxTileX >= utils.WplaceTilesPerEdge || maxTileY >= utils.WplaceTilesPerEdge {
			return followupMessage(s, i, fmt.Sprintf("❌ Regionタイル範囲が無効です: X[%d-%d] Y[%d-%d]", minTileX, maxTileX, minTileY, maxTileY))
		}
		gridCols := maxTileX - minTileX + 1
		gridRows := maxTileY - minTileY + 1
		if gridCols <= 0 || gridRows <= 0 {
			return followupMessage(s, i, "❌ Regionタイル範囲が無効です。")
		}
		totalTiles := gridCols * gridRows
		if totalTiles > maxTilesLimit {
			return followupMessage(s, i, fmt.Sprintf("❌ 指定されたRegionが大きすぎます（%dタイル）。最大%dタイルまで取得可能です。", totalTiles, maxTilesLimit))
		}

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tilesData, err := c.downloadTilesGrid(ctx, minTileX, minTileY, gridCols, gridRows)
		cancel()
		if err != nil {
			return followupMessage(s, i, fmt.Sprintf("❌ タイル画像のダウンロードに失敗しました: %v", err))
		}

		buf, err := combineTiles(tilesData, utils.WplaceTileSize, utils.WplaceTileSize, gridCols, gridRows)
		if err != nil {
			return followupMessage(s, i, fmt.Sprintf("❌ 画像結合に失敗しました: %v", err))
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
					Value:  fmt.Sprintf("%dx%dpx (%d×%dpx)", imageWidth, imageHeight, imageWidth, imageHeight),
					Inline: true,
				},
				{
					Name:   "タイル数",
					Value:  fmt.Sprintf("%dタイル (%d×%d)", gridCols*gridRows, gridCols, gridRows),
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
		return sendImageFollowup(s, i, buf.Bytes(), filename, embed)
	}

	if fullsize != "" {
		if err := respondDeferred(s, i); err != nil {
			return err
		}
		imageData, filename, embed, err := c.buildFullsizeResult(fullsize, "")
		if err != nil {
			return followupMessage(s, i, "❌ "+err.Error())
		}
		return sendImageFollowup(s, i, imageData, filename, embed)
	}

	return respondGet(s, i, "❌ 座標またはRegion名を指定してください。coords, region, fullsize のいずれかを指定")
}

func (c *GetCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        "get",
		Description: "画像やデータを取得します。",
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "coords",
				Description: "タイル座標 (例: 1818-806)",
				Required:    false,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "region",
				Description: "Region名 (例: Tokyo#1)",
				Required:    false,
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "fullsize",
				Description: "フルサイズ取得: 6要素 1818-806-989-358-107-142 / 8要素 1818-806-989-358-1818-806-1096-500",
				Required:    false,
			},
		},
	}
}
