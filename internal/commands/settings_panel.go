package commands

import (
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/embeds"
	"Koukyo_discord_bot/internal/notifications"
	"fmt"
	"log"
	"strconv"

	"github.com/bwmarrin/discordgo"
)

// SettingsView 設定パネルのUIコンポーネント
type SettingsView struct {
	settings *config.SettingsManager
	notifier *notifications.Notifier
	guildID  string
}

// NewSettingsView 設定ビューを作成
func NewSettingsView(settings *config.SettingsManager, notifier *notifications.Notifier, guildID string) *SettingsView {
	return &SettingsView{
		settings: settings,
		notifier: notifier,
		guildID:  guildID,
	}
}

// Components UIコンポーネントを取得
func (v *SettingsView) Components() []discordgo.MessageComponent {
	settings := v.settings.GetGuildSettings(v.guildID)

	// 自動通知ボタン
	toggleLabel := "自動通知をオンにする"
	toggleStyle := discordgo.PrimaryButton
	if settings.AutoNotifyEnabled {
		toggleLabel = "自動通知をオフにする"
		toggleStyle = discordgo.DangerButton
	}

	// 通知指標ボタン
	metricLabel := "通知指標: 全体差分率"
	metricStyle := discordgo.SecondaryButton
	if settings.NotificationMetric == "weighted" {
		metricLabel = "通知指標: 加重差分率 (菊重視)"
		metricStyle = discordgo.PrimaryButton
	}

	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    toggleLabel,
					Style:    toggleStyle,
					CustomID: "settings_toggle_notify",
				},
				discordgo.Button{
					Label:    metricLabel,
					Style:    metricStyle,
					CustomID: "settings_toggle_metric",
				},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "通知閾値を設定",
					Style:    discordgo.SecondaryButton,
					CustomID: "settings_set_threshold",
				},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "メンション閾値を設定",
					Style:    discordgo.SecondaryButton,
					CustomID: "settings_set_mention_threshold",
				},
				discordgo.Button{
					Label:    "このチャンネルを通知先に設定",
					Style:    discordgo.SecondaryButton,
					CustomID: "settings_set_channel",
				},
			},
		},
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					Label:    "メンションロールを設定",
					Style:    discordgo.SecondaryButton,
					CustomID: "settings_set_mention_role",
				},
			},
		},
	}
}

// HandleSettingsButtonInteraction ボタン押下を処理
func HandleSettingsButtonInteraction(
	s *discordgo.Session,
	i *discordgo.InteractionCreate,
	settings *config.SettingsManager,
	notifier *notifications.Notifier,
) {
	// 権限チェック
	if !isAdmin(s, i.GuildID, interactionUserID(i)) {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ このパネルは管理者のみ操作できます。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	customID := i.MessageComponentData().CustomID

	switch customID {
	case "settings_toggle_notify":
		handleToggleNotify(s, i, settings, notifier)
	case "settings_toggle_metric":
		handleToggleMetric(s, i, settings, notifier)
	case "settings_set_threshold":
		handleSetThreshold(s, i, settings)
	case "settings_set_mention_threshold":
		handleSetMentionThreshold(s, i, settings)
	case "settings_set_channel":
		handleSetChannel(s, i, settings)
	case "settings_set_mention_role":
		handleSetMentionRole(s, i, settings)
	}
}

// handleToggleNotify 自動通知ON/OFF切り替え
func handleToggleNotify(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager, notifier *notifications.Notifier) {
	settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		gs.AutoNotifyEnabled = !gs.AutoNotifyEnabled
	})

	// 通知状態をリセット
	notifier.ResetState(i.GuildID)

	// Embedを更新
	embed := embeds.BuildSettingsEmbed(settings, i.GuildID)
	view := NewSettingsView(settings, notifier, i.GuildID)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: view.Components(),
		},
	})
}

// handleToggleMetric 通知指標切り替え
func handleToggleMetric(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager, notifier *notifications.Notifier) {
	settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		if gs.NotificationMetric == "overall" {
			gs.NotificationMetric = "weighted"
		} else {
			gs.NotificationMetric = "overall"
		}
	})

	// 通知状態をリセット
	notifier.ResetState(i.GuildID)

	// Embedを更新
	embed := embeds.BuildSettingsEmbed(settings, i.GuildID)
	view := NewSettingsView(settings, notifier, i.GuildID)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Components: view.Components(),
		},
	})
}

// handleSetThreshold 通知閾値設定モーダル
func handleSetThreshold(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager) {
	currentSettings := settings.GetGuildSettings(i.GuildID)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: "modal_set_threshold",
			Title:    "通知閾値を設定",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "threshold_input",
							Label:       "通知閾値（%）",
							Style:       discordgo.TextInputShort,
							Placeholder: "10",
							Value:       fmt.Sprintf("%.0f", currentSettings.NotificationThreshold),
							Required:    true,
							MinLength:   1,
							MaxLength:   5,
						},
					},
				},
			},
		},
	})
}

// handleSetMentionThreshold メンション閾値設定モーダル
func handleSetMentionThreshold(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager) {
	currentSettings := settings.GetGuildSettings(i.GuildID)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseModal,
		Data: &discordgo.InteractionResponseData{
			CustomID: "modal_set_mention_threshold",
			Title:    "メンション閾値を設定",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.TextInput{
							CustomID:    "mention_threshold_input",
							Label:       "メンション閾値（%）",
							Style:       discordgo.TextInputShort,
							Placeholder: "50",
							Value:       fmt.Sprintf("%.0f", currentSettings.MentionThreshold),
							Required:    true,
							MinLength:   1,
							MaxLength:   5,
						},
					},
				},
			},
		},
	})
}

// handleSetChannel 通知チャンネル設定
func handleSetChannel(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager) {
	channelID := i.ChannelID

	settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		gs.NotificationChannel = &channelID
	})

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "✅ このチャンネルを、このサーバーの通知先として設定しました。",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

// handleSetMentionRole メンションロール設定（セレクトメニュー）
func handleSetMentionRole(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager) {
	// サーバーのロール一覧を取得
	roles, err := s.GuildRoles(i.GuildID)
	if err != nil {
		log.Printf("Failed to get roles: %v", err)
		return
	}

	// @everyoneを除外して、セレクトメニューのオプションを作成
	options := []discordgo.SelectMenuOption{}
	for _, role := range roles {
		if role.ID == i.GuildID { // @everyone
			continue
		}
		options = append(options, discordgo.SelectMenuOption{
			Label: role.Name,
			Value: role.ID,
		})
	}

	// 最大25個まで
	if len(options) > 25 {
		options = options[:25]
	}

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "メンションするロールを選択してください:",
			Components: []discordgo.MessageComponent{
				discordgo.ActionsRow{
					Components: []discordgo.MessageComponent{
						discordgo.SelectMenu{
							CustomID:    "select_mention_role",
							Placeholder: "ロールを選択...",
							Options:     options,
						},
					},
				},
			},
			Flags: discordgo.MessageFlagsEphemeral,
		},
	})
}

// HandleSettingsModalSubmit モーダル送信を処理
func HandleSettingsModalSubmit(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager, notifier *notifications.Notifier) {
	data := i.ModalSubmitData()

	switch data.CustomID {
	case "modal_set_threshold":
		handleModalSetThreshold(s, i, settings, notifier, data)
	case "modal_set_mention_threshold":
		handleModalSetMentionThreshold(s, i, settings, data)
	}
}

func handleModalSetThreshold(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager, notifier *notifications.Notifier, data discordgo.ModalSubmitInteractionData) {
	input := data.Components[0].(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput).Value
	value, err := strconv.ParseFloat(input, 64)
	if err != nil || value < 0 || value > 100 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ 0〜100の数値を入力してください。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		gs.NotificationThreshold = value
	})

	notifier.ResetState(i.GuildID)

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("✅ 通知閾値を %.0f%% に設定しました。", value),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

func handleModalSetMentionThreshold(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager, data discordgo.ModalSubmitInteractionData) {
	input := data.Components[0].(*discordgo.ActionsRow).Components[0].(*discordgo.TextInput).Value
	value, err := strconv.ParseFloat(input, 64)
	if err != nil || value < 0 || value > 100 {
		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "❌ 0〜100の数値を入力してください。",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		})
		return
	}

	settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
		gs.MentionThreshold = value
	})

	s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: fmt.Sprintf("✅ メンション閾値を %.0f%% に設定しました。", value),
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	})
}

// HandleSettingsSelectMenu セレクトメニュー選択を処理
func HandleSettingsSelectMenu(s *discordgo.Session, i *discordgo.InteractionCreate, settings *config.SettingsManager) {
	data := i.MessageComponentData()

	if data.CustomID == "select_mention_role" {
		roleID := data.Values[0]

		settings.UpdateGuildSetting(i.GuildID, func(gs *config.GuildSettings) {
			gs.MentionRole = &roleID
		})

		s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{
				Content:    fmt.Sprintf("✅ メンションロールを <@&%s> に設定しました。", roleID),
				Components: []discordgo.MessageComponent{}, // メニューを削除
			},
		})
	}
}
