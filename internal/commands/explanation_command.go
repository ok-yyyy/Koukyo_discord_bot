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
					"- 5秒ごとに Wplace のタイルを取得してテンプレートと比較し、差分率/差分px/加重差分率を `MonitorState` に保存します。",
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
				Name: "監視データの内容",
				Value: strings.Join([]string{
					"テンプレートの不透明ピクセルのうち、RGB差の合計が45超 または アルファ差が15超のものを差分とします。",
					"- 差分率 / 差分px",
					"- 加重差分率（菊全体と背景全体を同じ重みで計算）",
					"- 菊/背景それぞれの差分pxと総px",
					"",
					"結果は `MonitorState.LatestData` に保存し、通知/統計/コマンドが参照します。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "監視画像",
				Value: strings.Join([]string{
					"取得のたびに2枚のPNGを生成します:",
					"- live画像: テンプレートの範囲で切り抜いた現在の状態",
					"- diff画像: 差分ピクセルを赤で着色",
					"",
					"画像は `MonitorState.LatestImages` に保存され、通知の結合プレビューに使われます。",
				}, "\n"),
				Inline: false,
			},
			{
				Name: "0%継続時の扱い",
				Value: strings.Join([]string{
					"- 完全0%が続く間、差分履歴は1分間隔に間引いて保存します。",
					"- 完全0%が10分継続すると、small diff 通知の編集先メッセージをリセットします。",
					"- 通知・ヒートマップ集計・ユーザー活動の追跡は常時動作します。",
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
