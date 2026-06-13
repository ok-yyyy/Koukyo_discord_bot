package notifications

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	wplaceHealthURL      = "https://backend.wplace.live/health"
	wplaceHealthInterval = 3 * time.Minute
	wplaceHealthTimeout  = 10 * time.Second
	// HTTP失敗がこの回数連続したら障害とみなす（一時的なネットワーク揺らぎを除外）
	wplaceConsecFailsMax = 2
)

// wplaceHealthState はグローバルな障害追跡状態。Notifier に埋め込む。
type wplaceHealthState struct {
	mu          sync.Mutex
	outageSince time.Time // ゼロ値 = 正常
	consecFails int
	alerted     bool // 障害通知を送信済みか
}

type wplaceHealthResponse struct {
	Database bool   `json:"database"`
	Up       bool   `json:"up"`
	Uptime   string `json:"uptime"`
}

func (n *Notifier) startWplaceHealthLoop() {
	client := &http.Client{Timeout: wplaceHealthTimeout}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("PANIC in wplaceHealthLoop: %v", r)
			}
		}()

		// 起動直後はbotが安定するまで少し待つ
		time.Sleep(30 * time.Second)

		// 初回チェック
		n.checkWplaceHealth(client)

		ticker := time.NewTicker(wplaceHealthInterval)
		defer ticker.Stop()
		for range ticker.C {
			n.checkWplaceHealth(client)
		}
	}()
}

func (n *Notifier) checkWplaceHealth(client *http.Client) {
	resp, err := fetchWplaceHealth(client)

	n.wplaceHealth.mu.Lock()
	defer n.wplaceHealth.mu.Unlock()

	if err != nil || (resp != nil && (!resp.Up || !resp.Database)) {
		// 失敗
		n.wplaceHealth.consecFails++
		// 障害開始時刻は最初の失敗から記録する（通知タイミングではなく実際の障害開始）
		if n.wplaceHealth.outageSince.IsZero() {
			n.wplaceHealth.outageSince = time.Now()
		}
		reason := wplaceFailReason(resp, err)
		log.Printf("wplace health check failed (%d/%d): %s",
			n.wplaceHealth.consecFails, wplaceConsecFailsMax, reason)

		if n.wplaceHealth.consecFails >= wplaceConsecFailsMax && !n.wplaceHealth.alerted {
			n.wplaceHealth.alerted = true
			go n.notifyWplaceOutage(reason)
		}
	} else {
		// 正常
		if n.wplaceHealth.alerted {
			since := n.wplaceHealth.outageSince
			go n.notifyWplaceRecovery(since)
		}
		n.wplaceHealth.consecFails = 0
		n.wplaceHealth.outageSince = time.Time{}
		n.wplaceHealth.alerted = false
	}
}

func wplaceFailReason(resp *wplaceHealthResponse, err error) string {
	if err != nil {
		return fmt.Sprintf("接続失敗: %v", err)
	}
	if !resp.Up {
		return "サービス停止 (up: false)"
	}
	return "データベース異常 (database: false)"
}

func fetchWplaceHealth(client *http.Client) (*wplaceHealthResponse, error) {
	req, err := http.NewRequest(http.MethodGet, wplaceHealthURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; KoukyoBot/1.0)")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body) //nolint:errcheck
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	var h wplaceHealthResponse
	if err := json.Unmarshal(body, &h); err != nil {
		return nil, fmt.Errorf("parse error: %w", err)
	}
	return &h, nil
}

func (n *Notifier) notifyWplaceOutage(reason string) {
	embed := &discordgo.MessageEmbed{
		Title:       "⚠️ wplace 障害検知",
		Description: "wplace.live が正常に応答していません。",
		Color:       0xFF4444,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "原因", Value: reason, Inline: false},
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	n.broadcastToNotificationChannels(embed)
}

func (n *Notifier) notifyWplaceRecovery(since time.Time) {
	downtime := time.Since(since)
	embed := &discordgo.MessageEmbed{
		Title:       "✅ wplace 復旧",
		Description: "wplace.live が復旧しました。",
		Color:       0x44BB44,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "ダウン時間", Value: wplaceFormatDuration(downtime), Inline: true},
		},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
	n.broadcastToNotificationChannels(embed)
}

func (n *Notifier) broadcastToNotificationChannels(embed *discordgo.MessageEmbed) {
	for _, guild := range n.session.State.Guilds {
		gs := n.settings.GetGuildSettings(guild.ID)
		if !gs.AutoNotifyEnabled || gs.NotificationChannel == nil {
			continue
		}
		_, err := n.session.ChannelMessageSendEmbed(*gs.NotificationChannel, embed)
		if err != nil {
			log.Printf("wplace health: notify guild %s: %v", guild.ID, err)
		}
	}
}

func wplaceFormatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分%d秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
