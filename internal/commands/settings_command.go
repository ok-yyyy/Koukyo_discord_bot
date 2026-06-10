package commands

import (
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/notifications"
	"log"

	"github.com/bwmarrin/discordgo"
)

// SettingsCommand 設定コマンド
type SettingsCommand struct {
	settings *config.SettingsManager
	notifier *notifications.Notifier
}

// NewSettingsCommand 設定コマンドを作成
func NewSettingsCommand(settings *config.SettingsManager, notifier *notifications.Notifier) *SettingsCommand {
	return &SettingsCommand{
		settings: settings,
		notifier: notifier,
	}
}

func (c *SettingsCommand) Name() string { return "settings" }
func (c *SettingsCommand) Description() string {
	return "Botの設定パネルを開きます（管理者専用）"
}

func (c *SettingsCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
	}
}

// ExecuteText テキストコマンド実行（管理者のみ）
func (c *SettingsCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	// 権限チェック
	if !isAdmin(s, m.GuildID, m.Author.ID) {
		s.ChannelMessageSend(m.ChannelID, "❌ このコマンドは管理者のみ使用できます。")
		return nil
	}

	// Embedとボタンを作成
	embed := embeds.BuildSettingsEmbed(c.settings, m.GuildID)
	view := NewSettingsView(c.settings, c.notifier, m.GuildID)

	_, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Embed:      embed,
		Components: view.Components(),
	})

	return err
}

// ExecuteSlash スラッシュコマンド実行（管理者のみ）
func (c *SettingsCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	// 権限チェック
	if !isAdmin(s, i.GuildID, interactionUserID(i)) {
		return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このコマンドは管理者のみ使用できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
	}

	// Embedとボタンを作成
	embed := embeds.BuildSettingsEmbed(c.settings, i.GuildID)
	view := NewSettingsView(c.settings, c.notifier, i.GuildID)

	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: view.Components(),
			Flags:      discordgo.MessageFlagsEphemeral,
		},
	})
}

// isAdmin 管理者またはBotオーナーかチェック
func isAdmin(s *discordgo.Session, guildID, userID string) bool {
	// BotオーナーのID
	// TODO: 環境変数から設定できるようにする
	const OWNER_USER_ID = "1445347049611460642"

	if userID == OWNER_USER_ID {
		return true
	}

	// サーバー管理者権限をチェック
	member, err := s.GuildMember(guildID, userID)
	if err != nil {
		log.Printf("Failed to get member: %v", err)
		return false
	}

	guild, err := s.Guild(guildID)
	if err != nil {
		log.Printf("Failed to get guild: %v", err)
		return false
	}

	// サーバーオーナーの場合
	if guild.OwnerID == userID {
		return true
	}

	// 管理者権限を持つロールをチェック
	for _, roleID := range member.Roles {
		role, err := s.State.Role(guildID, roleID)
		if err != nil {
			continue
		}

		// 管理者権限 or チャンネル管理権限
		if (role.Permissions & discordgo.PermissionAdministrator) != 0 {
			return true
		}
		if (role.Permissions & discordgo.PermissionManageChannels) != 0 {
			return true
		}
	}

	return false
}
