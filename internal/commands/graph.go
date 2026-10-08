package commands

import (
	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/monitor"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

var commandJST = time.FixedZone("JST", 9*3600)

// GraphCommand 差分率のグラフ表示
type GraphCommand struct {
	mon     *monitor.Monitor
	dataDir string
}

func NewGraphCommand(mon *monitor.Monitor, dataDir string) *GraphCommand {
	return &GraphCommand{mon: mon, dataDir: dataDir}
}

func (c *GraphCommand) Name() string { return "graph" }
func (c *GraphCommand) Description() string {
	return "差分率の時系列グラフを表示します（オプションで期間指定可）"
}

// executeDiff は、差分率グラフ生成の共通ロジック
func (c *GraphCommand) executeDiff(duration time.Duration) (*discordgo.MessageEmbed, *bytes.Buffer, error) {
	if c.mon == nil {
		return nil, nil, fmt.Errorf("graphでエラーが発生しました: 監視システムが初期化されていません。")
	}
	if !c.mon.State.HasData() {
		return nil, nil, fmt.Errorf("graphでエラーが発生しました: 監視データがまだ受信できていません。")
	}

	history := c.mon.State.GetDiffHistory(duration)
	pngBuf, err := embeds.BuildDiffGraphPNG(history)
	if err != nil {
		return nil, nil, fmt.Errorf("グラフ生成に失敗しました: %w", err)
	}

	title := "差分率グラフ"
	nowJST := time.Now().In(commandJST)
	embed := &discordgo.MessageEmbed{
		Title:       title,
		Description: fmt.Sprintf("範囲: %s / データ点: %d / 時刻軸: JST", humanDuration(duration), len(history)),
		Color:       0x63A4FF,
		Timestamp:   nowJST.Format(time.RFC3339),
		Image:       &discordgo.MessageEmbedImage{URL: "attachment://diff_graph.png"},
	}

	return embed, pngBuf, nil
}

// executeVandal は、日次荒らし件数グラフ生成の共通ロジック
func (c *GraphCommand) executeVandal(duration time.Duration) (*discordgo.MessageEmbed, *bytes.Buffer, error) {
	if c.dataDir == "" {
		return nil, nil, fmt.Errorf("graphでエラーが発生しました: dataDirが未設定です。")
	}
	days := int(duration.Hours()/24 + 0.5)
	if days < 1 {
		days = 7
	}
	if days > 60 {
		days = 60
	}
	labels, counts, err := buildDailyVandalCounts(c.dataDir, days)
	if err != nil {
		return nil, nil, fmt.Errorf("graphでエラーが発生しました: %w", err)
	}
	pngBuf, err := embeds.BuildDailyCountGraphPNG(labels, counts)
	if err != nil {
		return nil, nil, fmt.Errorf("グラフ生成に失敗しました: %w", err)
	}
	nowJST := time.Now().In(commandJST)
	embed := &discordgo.MessageEmbed{
		Title:       "荒らし件数グラフ",
		Description: fmt.Sprintf("範囲: 過去%d日(JST) / データ点: %d", days, len(labels)),
		Color:       0xE74C3C,
		Timestamp:   nowJST.Format(time.RFC3339),
		Image:       &discordgo.MessageEmbedImage{URL: "attachment://vandal_graph.png"},
	}
	return embed, pngBuf, nil
}

func (c *GraphCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	graphType := "diff"
	duration := 1 * time.Hour

	// 引数: type=diff|vandal, duration=30m|1h|6h|24h
	for _, a := range args {
		if strings.HasPrefix(a, "type=") {
			graphType = strings.TrimPrefix(a, "type=")
		} else if strings.HasPrefix(a, "duration=") {
			v := strings.TrimPrefix(a, "duration=")
			if d, err := parseDuration(v); err == nil {
				duration = d
			}
		}
	}

	var (
		embed  *discordgo.MessageEmbed
		pngBuf *bytes.Buffer
		err    error
	)
	if graphType == "vandal" {
		embed, pngBuf, err = c.executeVandal(duration)
	} else {
		embed, pngBuf, err = c.executeDiff(duration)
	}
	if err != nil {
		_, e := s.ChannelMessageSend(m.ChannelID, err.Error())
		return e
	}

	fileName := "diff_graph.png"
	if embed.Image != nil && strings.HasPrefix(embed.Image.URL, "attachment://") {
		fileName = strings.TrimPrefix(embed.Image.URL, "attachment://")
	}
	_, err = s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Embeds: []*discordgo.MessageEmbed{embed},
		Files: []*discordgo.File{{
			Name:        fileName,
			ContentType: "image/png",
			Reader:      pngBuf,
		}},
	})
	return err
}

func (c *GraphCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	graphType := "diff"
	duration := 1 * time.Hour
	opts := i.ApplicationCommandData().Options
	for _, opt := range opts {
		switch opt.Name {
		case "type":
			graphType = opt.StringValue()
		case "duration":
			v := opt.StringValue()
			if d, err := parseDuration(v); err == nil {
				duration = d
			}
		}
	}

	var (
		embed  *discordgo.MessageEmbed
		pngBuf *bytes.Buffer
		err    error
	)
	if graphType == "vandal" {
		embed, pngBuf, err = c.executeVandal(duration)
	} else {
		embed, pngBuf, err = c.executeDiff(duration)
	}
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
			Files: []*discordgo.File{{
				Name:        embed.Image.URL[len("attachment://"):],
				ContentType: "image/png",
				Reader:      pngBuf,
			}},
		},
	})
}

func (c *GraphCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "type",
				Description: "グラフ種別: diff | vandal",
				Required:    false,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "diff", Value: "diff"},
					{Name: "vandal", Value: "vandal"},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "duration",
				Description: "範囲: 30m | 1h | 6h | 24h",
				Required:    false,
			},
		},
	}
}

func parseDuration(s string) (time.Duration, error) {
	switch s {
	case "30m":
		return 30 * time.Minute, nil
	case "1h":
		return 1 * time.Hour, nil
	case "6h":
		return 6 * time.Hour, nil
	case "24h":
		return 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func humanDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func buildDailyVandalCounts(dataDir string, days int) ([]string, []int, error) {
	countsByDate := make(map[string]int)
	dailyPath := filepath.Join(dataDir, "vandal_daily.json")
	if data, err := os.ReadFile(dailyPath); err == nil {
		var daily activity.DailyPixelCounts
		if err := json.Unmarshal(data, &daily); err == nil && daily.Vandal != nil {
			for dateKey, count := range daily.Vandal {
				countsByDate[dateKey] += count
			}
		}
	}

	if len(countsByDate) == 0 {
		path := filepath.Join(dataDir, "user_activity.json")
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nil, fmt.Errorf("user_activity.jsonの読み込みに失敗しました: %w", err)
		}
		var raw map[string]*activity.UserActivity
		if err := json.Unmarshal(data, &raw); err != nil {
			return nil, nil, fmt.Errorf("user_activity.jsonの解析に失敗しました: %w", err)
		}
		for _, entry := range raw {
			for dateKey, count := range entry.DailyVandalCounts {
				countsByDate[dateKey] += count
			}
		}
	}

	jst := time.FixedZone("JST", 9*3600)
	now := time.Now().In(jst)
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, jst).AddDate(0, 0, -(days - 1))

	labels := make([]string, 0, days)
	counts := make([]int, 0, days)
	for i := 0; i < days; i++ {
		d := start.AddDate(0, 0, i)
		key := d.Format("2006-01-02")
		labels = append(labels, d.Format("01-02"))
		counts = append(counts, countsByDate[key])
	}
	return labels, counts, nil
}
