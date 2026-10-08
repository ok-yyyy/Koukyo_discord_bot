package commands

import (
	"Koukyo_discord_bot/internal/monitor"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	predictDefaultDuration = 30 * time.Minute
	predictMinSlopeEpsilon = 1e-9
)

// PredictCommand 現在の修復速度から完全修復までの予測時間を表示
type PredictCommand struct {
	mon *monitor.Monitor
}

func NewPredictCommand(mon *monitor.Monitor) *PredictCommand {
	return &PredictCommand{mon: mon}
}

func (c *PredictCommand) Name() string { return "predict" }

func (c *PredictCommand) Description() string {
	return "現在の修復速度から完全修復までの推定時間を表示します"
}

func (c *PredictCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	duration := predictDefaultDuration
	for _, a := range args {
		if strings.HasPrefix(a, "duration=") {
			raw := strings.TrimSpace(strings.TrimPrefix(a, "duration="))
			d, err := parseDuration(raw)
			if err != nil || d <= 0 {
				_, sendErr := s.ChannelMessageSend(m.ChannelID, "❌ duration の形式が不正です。例: `duration=30m`, `duration=1h`")
				return sendErr
			}
			duration = d
		}
	}

	embed, err := c.buildPredictionEmbed(duration)
	if err != nil {
		_, sendErr := s.ChannelMessageSend(m.ChannelID, err.Error())
		return sendErr
	}
	_, err = s.ChannelMessageSendEmbed(m.ChannelID, embed)
	return err
}

func (c *PredictCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	duration := predictDefaultDuration

	for _, opt := range i.ApplicationCommandData().Options {
		switch opt.Name {
		case "duration":
			d, err := parseDuration(opt.StringValue())
			if err != nil || d <= 0 {
				return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
					Type: discordgo.InteractionResponseChannelMessageWithSource,
					Data: &discordgo.InteractionResponseData{
						Content: "❌ duration の形式が不正です。例: `30m`, `1h`",
						Flags:   discordgo.MessageFlagsEphemeral,
					},
				})
			}
			duration = d
		}
	}

	embed, err := c.buildPredictionEmbed(duration)
	if err != nil {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: err.Error(),
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds: []*discordgo.MessageEmbed{embed},
		},
	})
}

func (c *PredictCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "duration",
				Description: "観測窓 (例: 10m, 30m, 1h, 3h, 6h, 24h)",
				Required:    false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "10m", Value: "10m"},
					{Name: "30m", Value: "30m"},
					{Name: "1h", Value: "1h"},
					{Name: "3h", Value: "3h"},
					{Name: "6h", Value: "6h"},
					{Name: "24h", Value: "24h"},
				},
			},
		},
	}
}

func (c *PredictCommand) buildPredictionEmbed(duration time.Duration) (*discordgo.MessageEmbed, error) {
	if c.mon == nil {
		return nil, fmt.Errorf("❌ predictでエラーが発生しました: 監視システムが初期化されていません。")
	}
	if !c.mon.State.HasData() {
		return nil, fmt.Errorf("❌ predictでエラーが発生しました: 監視データがまだ受信できていません。")
	}

	data := c.mon.State.GetLatestData()
	if data == nil {
		return nil, fmt.Errorf("❌ predictでエラーが発生しました: 監視データが取得できませんでした。")
	}

	current := data.DiffPercentage
	history := c.mon.State.GetDiffHistory(duration)
	history = sanitizePredictHistory(history)

	nowJST := time.Now().In(commandJST)
	embed := &discordgo.MessageEmbed{
		Title:       "🔮 修復予測",
		Description: "現在の差分率と直近データから、完全修復(0.00%)までの時間を推定します。",
		Color:       0x3498DB,
		Timestamp:   nowJST.Format(time.RFC3339),
		Fields: []*discordgo.MessageEmbedField{
			{
				Name:   "現在値",
				Value:  fmt.Sprintf("%.3f%%", current),
				Inline: true,
			},
			{
				Name:   "観測窓",
				Value:  humanDuration(duration),
				Inline: true,
			},
			{
				Name:   "サンプル数",
				Value:  fmt.Sprintf("%d", len(history)),
				Inline: true,
			},
		},
	}

	if current <= 0.005 {
		embed.Color = 0x2ECC71
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "推定結果",
			Value:  "既に 0.00% 付近です。完全修復済みとみなせます。",
			Inline: false,
		})
		return embed, nil
	}

	repairRatePerSec, method, ok := estimateRepairRatePerSecond(history)
	if !ok {
		embed.Color = 0xE67E22
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "推定結果",
			Value:  "直近データから安定した修復速度を算出できませんでした（増加傾向またはデータ不足）。",
			Inline: false,
		})
		return embed, nil
	}

	etaSeconds := current / repairRatePerSec
	if math.IsNaN(etaSeconds) || math.IsInf(etaSeconds, 0) || etaSeconds <= 0 {
		embed.Color = 0xE67E22
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "推定結果",
			Value:  "推定時間を算出できませんでした。",
			Inline: false,
		})
		return embed, nil
	}

	eta := time.Duration(etaSeconds * float64(time.Second))
	finishAt := nowJST.Add(eta)
	speedPerMinute := repairRatePerSec * 60

	embed.Color = 0x2ECC71
	embed.Fields = append(embed.Fields,
		&discordgo.MessageEmbedField{
			Name:   "推定修復速度",
			Value:  fmt.Sprintf("%.4f%% / 分", speedPerMinute),
			Inline: true,
		},
		&discordgo.MessageEmbedField{
			Name:   "完全修復まで",
			Value:  formatPredictETA(eta),
			Inline: true,
		},
		&discordgo.MessageEmbedField{
			Name:   "推定完了時刻 (JST)",
			Value:  finishAt.Format("2006-01-02 15:04:05"),
			Inline: true,
		},
		&discordgo.MessageEmbedField{
			Name:   "算出方式",
			Value:  method,
			Inline: true,
		},
	)

	if data.TotalPixels > 0 {
		pxPerMinute := speedPerMinute * float64(data.TotalPixels) / 100.0
		embed.Fields = append(embed.Fields, &discordgo.MessageEmbedField{
			Name:   "推定修復速度 (px)",
			Value:  fmt.Sprintf("%.1f px / 分", pxPerMinute),
			Inline: true,
		})
	}

	return embed, nil
}

func sanitizePredictHistory(history []monitor.DiffRecord) []monitor.DiffRecord {
	if len(history) == 0 {
		return nil
	}
	out := make([]monitor.DiffRecord, 0, len(history))
	for _, r := range history {
		if r.Timestamp.IsZero() {
			continue
		}
		if math.IsNaN(r.Percentage) || math.IsInf(r.Percentage, 0) {
			continue
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.Before(out[j].Timestamp)
	})
	return out
}

func estimateRepairRatePerSecond(history []monitor.DiffRecord) (rate float64, method string, ok bool) {
	if len(history) < 2 {
		return 0, "", false
	}

	if slope, slopeOK := linearSlopePerSecond(history); slopeOK && slope < -predictMinSlopeEpsilon {
		return -slope, "線形回帰", true
	}

	first := history[0]
	last := history[len(history)-1]
	elapsedSec := last.Timestamp.Sub(first.Timestamp).Seconds()
	if elapsedSec <= 0 {
		return 0, "", false
	}
	netSlope := (last.Percentage - first.Percentage) / elapsedSec
	if netSlope < -predictMinSlopeEpsilon {
		return -netSlope, "始点終点の平均", true
	}
	return 0, "", false
}

func linearSlopePerSecond(history []monitor.DiffRecord) (float64, bool) {
	if len(history) < 2 {
		return 0, false
	}
	base := history[0].Timestamp
	var (
		sumX  float64
		sumY  float64
		sumXX float64
		sumXY float64
		n     float64
	)
	for _, r := range history {
		x := r.Timestamp.Sub(base).Seconds()
		if x < 0 {
			continue
		}
		y := r.Percentage
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
		n++
	}
	if n < 2 {
		return 0, false
	}
	denom := n*sumXX - sumX*sumX
	if math.Abs(denom) < 1e-12 {
		return 0, false
	}
	slope := (n*sumXY - sumX*sumY) / denom
	return slope, true
}

func formatPredictETA(d time.Duration) string {
	if d <= 0 {
		return "0秒"
	}
	totalSec := int(d.Round(time.Second).Seconds())
	days := totalSec / (24 * 3600)
	totalSec %= 24 * 3600
	hours := totalSec / 3600
	totalSec %= 3600
	minutes := totalSec / 60
	seconds := totalSec % 60

	parts := make([]string, 0, 4)
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%d日", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d時間", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d分", minutes))
	}
	if seconds > 0 && len(parts) < 2 {
		parts = append(parts, fmt.Sprintf("%d秒", seconds))
	}
	return strings.Join(parts, " ")
}
