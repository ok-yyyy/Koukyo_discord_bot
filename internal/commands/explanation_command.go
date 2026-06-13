package commands

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"Koukyo_discord_bot/internal/version"

	"github.com/bwmarrin/discordgo"
)

const (
	explanationPagePrefix = "explanation_page:"
	explanationMaxPage    = 2
)

// ExplanationCommand explains the bot architecture and major subsystems.
// Slash: /explanation (ephemeral)
// Text: !explanation
type ExplanationCommand struct{}

func NewExplanationCommand() *ExplanationCommand { return &ExplanationCommand{} }

func (c *ExplanationCommand) Name() string { return "explanation" }

func (c *ExplanationCommand) Description() string {
	return "このBotのアーキテクチャ/通知ロジックを解説します"
}

func (c *ExplanationCommand) ExecuteText(s *discordgo.Session, m *discordgo.MessageCreate, args []string) error {
	embed := buildExplanationEmbed(1)
	_, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{embed},
		Components:      buildExplanationComponents(1),
		AllowedMentions: &discordgo.MessageAllowedMentions{},
	})
	return err
}

func (c *ExplanationCommand) ExecuteSlash(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	embed := buildExplanationEmbed(1)
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{embed},
			Flags:      discordgo.MessageFlagsEphemeral,
			Components: buildExplanationComponents(1),
		},
	})
}

func (c *ExplanationCommand) SlashDefinition() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{
		Name:        c.Name(),
		Description: c.Description(),
	}
}

func buildExplanationEmbed(page int) *discordgo.MessageEmbed {
	page = clampExplanationPage(page)
	// Keep it short; point to what to look at and how data flows.
	fields := []*discordgo.MessageEmbedField{}
	if page == 1 {
		fields = []*discordgo.MessageEmbedField{
			{
				Name: "1) 監視データの入口",
				Value: strings.Join([]string{
					"- WebSocket から差分率/差分px/加重差分率などを受信し、`MonitorState` に保存します。",
					"- 最新値(`LatestData`)と最新画像(`LatestImages`)が通知や /get に使われます。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "2) メイン通知フロー",
				Value: strings.Join([]string{
					"- 1秒ごとに全ギルドの設定を見て、差分の Tier(10/20/.../100) 変化時のみ通知します。",
					"- 指標は `差分率` / `加重差分率` をギルド設定で切り替えます。",
					"- Pixel Perfect(0%) に戻ったときは修復完了通知を出します。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "3) small-diff (<=10px) スパム抑制",
				Value: strings.Join([]string{
					"- 差分pxが少ない時は、1つのテキストメッセージを編集して追従します。",
					"- いったん 11px以上を検知したら、0%に戻るまで embed 通知フローに固定します（混在防止）。",
					"- 10px->11px の移行時は、しきい値未満でも 1回だけスナップショット embed を送ります。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "4) 追加監視 (watch_targets / progress_targets)",
				Value: strings.Join([]string{
					"- `data/watch_targets.json` / `data/progress_targets.json` + `data/template_img/` を元に、指定範囲を定期取得して差分/進捗を判定します。",
					"- タイル取得は /get と同じ結合ロジックを使い、キャッシュ回避クエリで新鮮な画像を取りに行きます。",
					"- `!{id}`（＋aliases）で手動取得できます。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "5) /get とタイル",
				Value: strings.Join([]string{
					"- Wplace のタイルは 1000x1000 PNG、全体は 2048x2048 タイルです。",
					"- 必要タイルを並列DLして結合し、指定範囲を切り抜いて返します（最大16タイル）。",
					"- 画像が古くなる問題があるため、タイルURLに `?t=` を付けてキャッシュを回避します。",
				}, "\n"),
				Inline: false,
			},
		}
	} else if page == 2 {
		fields = []*discordgo.MessageEmbedField{
			{
				Name: "監視データ(JSON)の形式",
				Value: strings.Join([]string{
					"WebSocketのテキストメッセージで受信する主なフィールド:",
					"`type`, `message`",
					"`diff_percentage`, `diff_pixels`",
					"`weighted_diff_percentage`, `weighted_diff_color`",
					"`chrysanthemum_diff_pixels`, `background_diff_pixels`",
					"`chrysanthemum_total_pixels`, `background_total_pixels`, `total_pixels`",
					"",
					"Bot側は `MonitorState.LatestData` に保存し、通知/統計/コマンドが参照します。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "監視画像(バイナリ)の形式",
				Value: strings.Join([]string{
					"WebSocketのバイナリは `type_id(1byte) + payload_size(4byte LE) + PNG`:",
					"- `type_id=2`: live画像",
					"- `type_id=3`: diff画像",
					"",
					"payloadの先頭に `00` が付くケースがあるのでBot側で除去してから保持します。",
					"画像は `MonitorState.LatestImages` に保存され、通知の結合プレビューに使われます。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "省電力モード(power_save_mode)",
				Value: strings.Join([]string{
					"- 完全0%が一定時間継続すると省電力になり、履歴保存や一部集計を止めます。",
					"- 通知側は `PowerSaveMode=true` の間はメイン通知をスキップします。",
					"- 復帰後は通常の差分通知ロジックに戻ります。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "“通知が詰まる” 典型原因",
				Value: strings.Join([]string{
					"- 画像結合(デコード/エンコード)や重い投稿(例: タイムラプス)を監視ループ内で同期実行すると、全体が止まって見えます。",
					"- small-diff編集とembed通知の混在は状態機械で明確に分離しています。",
				}, "\n"),
				Inline: false,
			},
		}
	}

	title := "🏯 Botアーキテクチャ解説"
	if page == 2 {
		title = "🏯 Botアーキテクチャ解説 (詳細)"
	}
	return &discordgo.MessageEmbed{
		Title:       title,
		Description: fmt.Sprintf("Version: `%s` | ページ %d/%d | 生成: `%s`", version.Version, page, explanationMaxPage, time.Now().Format("2006-01-02 15:04:05")),
		Color:       0x3498DB,
		Fields:      fields,
		Footer: &discordgo.MessageEmbedFooter{
			Text: "README.md / internal/monitor / internal/notifications を読むと追いやすいです",
		},
		Timestamp: time.Now().Format(time.RFC3339),
	}
}

func buildExplanationComponents(page int) []discordgo.MessageComponent {
	page = clampExplanationPage(page)
	prevDisabled := page <= 1
	nextDisabled := page >= explanationMaxPage
	return []discordgo.MessageComponent{
		discordgo.ActionsRow{
			Components: []discordgo.MessageComponent{
				discordgo.Button{
					CustomID: "explanation_page:" + strconv.Itoa(page-1),
					Label:    "Prev",
					Style:    discordgo.SecondaryButton,
					Disabled: prevDisabled,
				},
				discordgo.Button{
					CustomID: "explanation_page:" + strconv.Itoa(page+1),
					Label:    "Next",
					Style:    discordgo.PrimaryButton,
					Disabled: nextDisabled,
				},
			},
		},
	}
}

func clampExplanationPage(page int) int {
	if page < 1 {
		return 1
	}
	if page > explanationMaxPage {
		return explanationMaxPage
	}
	return page
}

func HandleExplanationPagination(s *discordgo.Session, i *discordgo.InteractionCreate) {
	customID := i.MessageComponentData().CustomID
	if !strings.HasPrefix(customID, explanationPagePrefix) {
		return
	}
	raw := strings.TrimPrefix(customID, explanationPagePrefix)
	page, err := strconv.Atoi(raw)
	if err != nil {
		page = 1
	}
	page = clampExplanationPage(page)

	embed := buildExplanationEmbed(page)
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:          []*discordgo.MessageEmbed{embed},
			Components:      buildExplanationComponents(page),
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		},
	})
}
