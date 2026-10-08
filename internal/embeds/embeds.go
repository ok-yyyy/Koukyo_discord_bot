package embeds

import (
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/models"
	"Koukyo_discord_bot/internal/monitor"
	"Koukyo_discord_bot/internal/utils"
	"fmt"
	"runtime"
	"time"

	"github.com/bwmarrin/discordgo"
)

// BuildConvertLngLatEmbed 経度緯度 → ピクセル座標変換結果の埋め込みを作成
func BuildConvertLngLatEmbed(lng, lat float64) *discordgo.MessageEmbed {
	coord := utils.LngLatToTilePixel(lng, lat)
	url := utils.BuildWplaceURL(lng, lat, 14.76)
	hyphenCoords := utils.FormatHyphenCoords(coord)

	embed := &discordgo.MessageEmbed{
		Title:       "🗺️ 座標変換: 経度緯度 → ピクセル座標",
		Description: fmt.Sprintf("**入力:** 経度 `%.6f`, 緯度 `%.6f`", lng, lat),
		Color:       0x9B59B6, // Purple
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📍 タイル座標",
				Value:  fmt.Sprintf("TlX: `%d`, TlY: `%d`", coord.TileX, coord.TileY),
				Inline: false,
			},
			{
				Name:   "🔲 ピクセル座標",
				Value:  fmt.Sprintf("PxX: `%d`, PxY: `%d`", coord.PixelX, coord.PixelY),
				Inline: false,
			},
			{
				Name:   "📋 ハイフン形式",
				Value:  fmt.Sprintf("`%s`", hyphenCoords),
				Inline: false,
			},
			{
				Name:   "🔗 Wplace URL",
				Value:  fmt.Sprintf("[地図を開く](%s)", url),
				Inline: false,
			},
		},
	}

	return embed
}

// BuildConvertPixelEmbed ピクセル座標 → 経度緯度変換結果の埋め込みを作成
func BuildConvertPixelEmbed(tileX, tileY, pixelX, pixelY int) *discordgo.MessageEmbed {
	lngLat := utils.TilePixelCenterToLngLat(tileX, tileY, pixelX, pixelY)
	url := utils.BuildWplaceURL(lngLat.Lng, lngLat.Lat, 14.76)
	coord := &utils.Coordinate{TileX: tileX, TileY: tileY, PixelX: pixelX, PixelY: pixelY}
	hyphenCoords := utils.FormatHyphenCoords(coord)

	embed := &discordgo.MessageEmbed{
		Title: "🗺️ 座標変換: ピクセル座標 → 経度緯度",
		Description: fmt.Sprintf("**入力:** TlX `%d`, TlY `%d`, PxX `%d`, PxY `%d`",
			tileX, tileY, pixelX, pixelY),
		Color: 0x1ABC9C, // Turquoise
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "🌐 経度緯度",
				Value:  fmt.Sprintf("経度: `%.6f`, 緯度: `%.6f`", lngLat.Lng, lngLat.Lat),
				Inline: false,
			},
			{
				Name:   "📋 ハイフン形式",
				Value:  fmt.Sprintf("`%s`", hyphenCoords),
				Inline: false,
			},
			{
				Name:   "🔗 Wplace URL",
				Value:  fmt.Sprintf("[地図を開く](%s)", url),
				Inline: false,
			},
		},
	}

	return embed
}

// BuildNowEmbed now コマンド用の埋め込みを作成
func BuildNowEmbed(mon *monitor.Monitor) *discordgo.MessageEmbed {
	now := time.Now()

	// JSTに変換（タイムゾーンデータがない場合はUTC+9で代用）
	jstLoc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		// タイムゾーンデータがない場合はUTC+9を使用
		jstLoc = time.FixedZone("JST", 9*60*60)
	}
	jstTime := now.In(jstLoc)

	// モニターがnilまたはデータがない場合
	if mon == nil || !mon.State.HasData() {
		embed := &discordgo.MessageEmbed{
			Title:       "🏯 Wplace 監視情報",
			Description: "**現在の監視状況**",
			Color:       0x3498DB, // Blue
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "📡 監視ステータス",
					Value:  "🔄 準備中（データ受信待機中）",
					Inline: false,
				},
				{
					Name:   "🎯 監視対象",
					Value:  "• 皇居エリア\n• 菊の紋章\n• 背景領域",
					Inline: true,
				},
				{
					Name:   "📊 実装予定機能",
					Value:  "• リアルタイム差分検知\n• 荒らし検出\n• 自動通知",
					Inline: true,
				},
				{
					Name:   "⏰ 現在時刻 (JST)",
					Value:  jstTime.Format("2006-01-02 15:04:05"),
					Inline: false,
				},
				{
					Name:   "ℹ️ 取得状態",
					Value:  getConnectionStatus(mon),
					Inline: false,
				},
			},
			Timestamp: now.UTC().Format(time.RFC3339),
			Footer: &discordgo.MessageEmbedFooter{
				Text: "監視システム起動中...",
			},
		}
		appendMainMonitorMapField(embed)
		return embed
	}

	// データがある場合
	data := mon.GetLatestData()
	if data == nil {
		embed := &discordgo.MessageEmbed{
			Title:       "🏯 Wplace 監視情報",
			Description: "**現在の監視状況**",
			Color:       0x3498DB,
			Fields: []*discordgo.MessageEmbedField{
				{
					Name:   "📡 監視ステータス",
					Value:  "🔄 準備中（データ受信待機中）",
					Inline: false,
				},
			},
			Timestamp: now.UTC().Format(time.RFC3339),
		}
		appendMainMonitorMapField(embed)
		return embed
	}

	// 差分率の表示
	diffValue := fmt.Sprintf("%.2f%%", data.DiffPercentage)
	if data.DiffPercentage == 0 {
		diffValue = "✅ **0.00%** (Pixel Perfect!)"
	}

	// 色の決定
	color := 0x2ECC71 // Green
	if data.DiffPercentage > 30 {
		color = 0xE74C3C // Red
	} else if data.DiffPercentage > 10 {
		color = 0xF39C12 // Orange
	}

	embed := &discordgo.MessageEmbed{
		Title:       "🏯 Wplace 監視情報",
		Description: "**現在の監視状況**",
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "📊 差分率",
				Value:  diffValue,
				Inline: false,
			},
			{
				Name:   "📈 差分ピクセル",
				Value:  fmt.Sprintf("%d / %d", data.DiffPixels, data.TotalPixels),
				Inline: false,
			},
		},
		Timestamp: data.Timestamp.UTC().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: fmt.Sprintf("最終更新 | データ件数: %d件", mon.State.GetDiffHistoryCount()),
		},
	}

	appendMainMonitorMapField(embed)

	return embed
}

func appendMainMonitorMapField(embed *discordgo.MessageEmbed) {
	if embed == nil {
		return
	}
	embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
		Name:   "Wplace.live",
		Value:  fmt.Sprintf("[地図で見る](%s)\n`/get fullsize:%s`", utils.BuildMainMonitorWplaceURL(), utils.MainMonitorFullsizeString()),
		Inline: false,
	})
}

// getConnectionStatus タイル取得の状態を取得
func getConnectionStatus(mon *monitor.Monitor) string {
	if mon == nil {
		return "⚠️ モニター未初期化"
	}
	if mon.IsHealthy() {
		return "✅ Wplaceからタイルを取得中"
	}
	return "⚠️ タイル取得を再試行中..."
}

// BuildStatusEmbed status コマンド用の詳細ステータス埋め込みを作成
func BuildStatusEmbed(botInfo *models.BotInfo, session *discordgo.Session) *discordgo.MessageEmbed {
	uptime := botInfo.Uptime()

	// サーバー数を取得
	guildCount := len(session.State.Guilds)

	// メモリ情報を取得
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	allocMB := float64(m.Alloc) / 1024 / 1024
	heapAllocMB := float64(m.HeapAlloc) / 1024 / 1024
	heapInuseMB := float64(m.HeapInuse) / 1024 / 1024
	sysMB := float64(m.Sys) / 1024 / 1024

	embed := &discordgo.MessageEmbed{
		Title:       "🤖 Bot ステータス",
		Description: "Bot自体のステータス（稼働時間、メモリ）",
		Color:       0x2ECC71, // Green
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "⏱️ 稼働時間",
				Value:  formatUptime(uptime),
				Inline: true,
			},
			{
				Name:   "🕐 起動時刻",
				Value:  botInfo.StartTime.Format("2006-01-02 15:04:05"),
				Inline: true,
			},
			{
				Name:   "💾 メモリ使用量",
				Value:  fmt.Sprintf("Alloc: %.2f MB\nHeapAlloc: %.2f MB\nHeapInuse: %.2f MB\nSys: %.2f MB", allocMB, heapAllocMB, heapInuseMB, sysMB),
				Inline: false,
			},
			{
				Name:   "📊 Goroutine数",
				Value:  fmt.Sprintf("%d", runtime.NumGoroutine()),
				Inline: true,
			},
			{
				Name:   "🏢 参加サーバー数",
				Value:  fmt.Sprintf("%d", guildCount),
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Koukyo Discord Bot - Go Edition",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}
	return embed
}

// formatUptime 稼働時間を人間が読みやすい形式にフォーマット
func formatUptime(d time.Duration) string {
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%d日 %d時間 %d分 %d秒", days, hours, minutes, seconds)
	} else if hours > 0 {
		return fmt.Sprintf("%d時間 %d分 %d秒", hours, minutes, seconds)
	} else if minutes > 0 {
		return fmt.Sprintf("%d分 %d秒", minutes, seconds)
	}
	return fmt.Sprintf("%d秒", seconds)
}

// BuildSettingsEmbed 設定パネル用のEmbedを作成
func BuildSettingsEmbed(settings *config.SettingsManager, guildID string) *discordgo.MessageEmbed {
	guildSettings := settings.GetGuildSettings(guildID)

	// 自動通知の状態
	notifyStatus := "❌ OFF"
	if guildSettings.AutoNotifyEnabled {
		notifyStatus = "✅ ON"
	}

	// 通知チャンネル
	channelText := "(未設定)"
	if guildSettings.NotificationChannel != nil {
		channelText = fmt.Sprintf("<#%s>", *guildSettings.NotificationChannel)
	}

	// メンションロール
	roleText := "(なし)"
	if guildSettings.MentionRole != nil {
		roleText = fmt.Sprintf("<@&%s>", *guildSettings.MentionRole)
	}
	achievementChannelText := "(未設定)"
	if guildSettings.AchievementChannel != nil {
		achievementChannelText = fmt.Sprintf("<#%s>", *guildSettings.AchievementChannel)
	}

	embed := &discordgo.MessageEmbed{
		Title:       "⚙️ Bot設定パネル",
		Description: "サーバーごとの通知設定を管理します",
		Color:       0x5865F2, // Discord Blurple
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "自動通知",
				Value:  fmt.Sprintf("**%s**", notifyStatus),
				Inline: false,
			},
			{
				Name:   "通知チャンネル",
				Value:  channelText,
				Inline: true,
			},
			{
				Name:   "通知閾値",
				Value:  fmt.Sprintf("**%.0f%%**", guildSettings.NotificationThreshold),
				Inline: true,
			},
			{
				Name:   "メンション閾値",
				Value:  fmt.Sprintf("**%.0f%%**", guildSettings.MentionThreshold),
				Inline: true,
			},
			{
				Name:   "メンションロール",
				Value:  roleText,
				Inline: true,
			},
			{
				Name:   "実績通知チャンネル",
				Value:  achievementChannelText,
				Inline: true,
			},
		},
		Footer: &discordgo.MessageEmbedFooter{
			Text: "ボタンをクリックして設定を変更できます",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}

	return embed
}
