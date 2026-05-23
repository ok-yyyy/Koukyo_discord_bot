package commands

import (
	"Koukyo_discord_bot/internal/notifications"
	"Koukyo_discord_bot/internal/utils" // 追加
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

type PaintCommand struct {
	notifier *notifications.Notifier
	timers   map[string]*time.Timer
	mu       sync.RWMutex
}

func NewPaintCommand(n *notifications.Notifier) *PaintCommand {
	return &PaintCommand{
		notifier: n,
		timers:   make(map[string]*time.Timer),
	}
}

func (c *PaintCommand) Name() string { return "paint" }
func (c *PaintCommand) Description() string {
	return "Paint回復時間の計算・通知設定を行います"
}

func (c *PaintCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	_, err := s.ChannelMessageSend(m.ChannelID, "このコマンドはスラッシュコマンドで利用してください。")
	return err
}

func (c *PaintCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	options := i.ApplicationCommandData().Options
	if len(options) == 0 {
		return respond(s, i, "❌ サブコマンドを指定してください (`set` または `cancel`)")
	}

	subcommand := options[0]
	switch subcommand.Name {
	case "set":
		return c.handleSet(s, i, subcommand.Options)
	case "cancel":
		return c.handleCancel(s, i)
	default:
		return respond(s, i, "❌ 未知のサブコマンドです")
	}
}

func (c *PaintCommand) handleSet(s *discordgo.Session, i *discordgo.InteractionCreate, options []*discordgo.ApplicationCommandInteractionDataOption) error {
	var current, max int
	notify := false
	selectedTimezone := "JST"
	userID := i.Member.User.ID

	for _, opt := range options {
		switch opt.Name {
		case "current":
			current = int(opt.IntValue())
		case "max":
			max = int(opt.IntValue())
		case "notify":
			if opt.StringValue() == "on" {
				notify = true
			}
		case "timezone":
			selectedTimezone = opt.StringValue()
		}
	}

	if current < 0 || max <= 0 || current > max {
		return respond(s, i, "❌ 入力値が不正です (今:0以上, 上限:1以上, 今≦上限)")
	}

	// タイムゾーンをロード
	loc, err := utils.ParseTimezone(selectedTimezone)
	if err != nil {
		return respond(s, i, fmt.Sprintf("❌ 無効なタイムゾーンが指定されました: %s", selectedTimezone))
	}

	nowInLoc := time.Now().In(loc)
	remain := max - current
	if remain == 0 {
		return respond(s, i, "🎉 すでに全回復しています！")
	}

	recoverSec := remain * 30
	duration := time.Duration(recoverSec) * time.Second
	finish := nowInLoc.Add(duration)

	msg := fmt.Sprintf(
		"🖌️ Paint回復計算\n残り: **%d** 回\n全回復まで: **%d分%d秒**\n全回復時刻: **%s (%s)**",
		remain,
		recoverSec/60,
		recoverSec%60,
		finish.Format("15:04:05"),
		finish.Format("MST"),
	)

	if notify {
		if c.notifier == nil {
			msg += "\n⚠️ 通知システムが利用できないため、通知予約はスキップされました。"
		} else {
			c.mu.Lock()
			// 既存のタイマーがあればキャンセル
			if oldTimer, ok := c.timers[userID]; ok {
				oldTimer.Stop()
			}

			// 新しいタイマーをセット
			c.timers[userID] = time.AfterFunc(duration, func() {
				c.mu.Lock()
				delete(c.timers, userID)
				c.mu.Unlock()
				c.notifier.EnqueueHigh(func() {
					c.notifier.NotifyPaintRecovery(userID, max)
				})
			})
			c.mu.Unlock()

			msg += "\n\n🔔 全回復時にDMで通知します！\n(キャンセルする場合は `/paint cancel` を実行してください)"
		}
	}

	return respond(s, i, msg)
}

func (c *PaintCommand) handleCancel(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	userID := i.Member.User.ID
	c.mu.Lock()
	defer c.mu.Unlock()

	if timer, ok := c.timers[userID]; ok {
		timer.Stop()
		delete(c.timers, userID)
		return respond(s, i, "✅ Paint回復通知の予約をキャンセルしました。")
	}

	return respond(s, i, "ℹ️ 予約されている通知はありません。")
}

func (c *PaintCommand) SlashDefinition() *discordgo.ApplicationCommand {
	commonTimezones := utils.GetCommonTimezones()
	timezoneChoices := []*discordgo.ApplicationCommandOptionChoice{}
	for _, tz := range commonTimezones {
		timezoneChoices = append(timezoneChoices, &discordgo.ApplicationCommandOptionChoice{
			Name:  fmt.Sprintf("%s (%s)", tz.Label, tz.Location.String()),
			Value: tz.Name,
		})
	}

	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "set",
				Description: "Paint回復時間の計算と通知設定を行います",
				Options: []*discordgo.ApplicationCommandOption{
					{
						Type:        discordgo.ApplicationCommandOptionInteger,
						Name:        "current",
						Description: "現在のPaint数",
						Required:    true,
					},
					{
						Type:        discordgo.ApplicationCommandOptionInteger,
						Name:        "max",
						Description: "Paint上限値",
						Required:    true,
					},
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "notify",
						Description: "全回復時に通知 (on/off)",
						Required:    false,
						Choices: []*discordgo.ApplicationCommandOptionChoice{
							{Name: "on", Value: "on"},
							{Name: "off", Value: "off"},
						},
					},
					{
						Type:        discordgo.ApplicationCommandOptionString,
						Name:        "timezone",
						Description: "タイムゾーン (デフォルト: JST)",
						Required:    false,
						Choices:     timezoneChoices,
					},
				},
			},
			{
				Type:        discordgo.ApplicationCommandOptionSubCommand,
				Name:        "cancel",
				Description: "設定されているPaint回復通知をキャンセルします",
			},
		},
	}
}

func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
		},
	})
}
