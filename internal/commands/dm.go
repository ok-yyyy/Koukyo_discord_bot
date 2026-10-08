package commands

import (
	"Koukyo_discord_bot/internal/config"

	"github.com/bwmarrin/discordgo"
)

// DMCommand /dm on|off でDM速報通知を有効/無効にするコマンド
type DMCommand struct {
	settings *config.SettingsManager
}

func NewDMCommand(settings *config.SettingsManager) *DMCommand {
	return &DMCommand{settings: settings}
}

func (c *DMCommand) Name() string { return "dm" }

func (c *DMCommand) Description() string {
	return "Wplace差分速報のDM通知を有効/無効にします（差分率10%以上で通知）"
}

func (c *DMCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	if len(args) == 0 {
		_, err := s.ChannelMessageSend(m.ChannelID, "使用方法: `!dm on` または `!dm off`")
		return err
	}
	return c.handleAction(args[0], m.Author.ID, func(msg string) error {
		_, err := s.ChannelMessageSend(m.ChannelID, msg)
		return err
	})
}

func (c *DMCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	userID := ""
	if i.Member != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}
	if userID == "" {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ ユーザーIDを取得できませんでした。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	action := ""
	for _, opt := range i.ApplicationCommandData().Options {
		if opt.Name == "action" {
			action = opt.StringValue()
		}
	}

	return c.handleAction(action, userID, func(msg string) error {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: msg,
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	})
}

func (c *DMCommand) handleAction(action, userID string, respond func(string) error) error {
	switch action {
	case "on":
		c.settings.SetUserDMEnabled(userID, true)
		return respond("✅ DM速報を有効にしました。差分率が10%以上になったときにDMでお知らせします。")
	case "off":
		c.settings.SetUserDMEnabled(userID, false)
		return respond("✅ DM速報を無効にしました。")
	default:
		return respond("❌ action は `on` または `off` を指定してください。")
	}
}

func (c *DMCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
		Options: []*discordgo.ApplicationCommandOption{
			{
				Type:        discordgo.ApplicationCommandOptionString,
				Name:        "action",
				Description: "on: 有効化 / off: 無効化",
				Required:    true,
				Choices: []*discordgo.ApplicationCommandOptionChoice{
					{Name: "on (有効)", Value: "on"},
					{Name: "off (無効)", Value: "off"},
				},
			},
		},
	}
}
