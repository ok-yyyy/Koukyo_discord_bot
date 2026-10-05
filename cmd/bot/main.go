package main

import (
	"Koukyo_discord_bot/internal/activity"
	"Koukyo_discord_bot/internal/config"
	"Koukyo_discord_bot/internal/handler"
	"Koukyo_discord_bot/internal/models"
	"Koukyo_discord_bot/internal/monitor"
	"Koukyo_discord_bot/internal/notifications"
	"Koukyo_discord_bot/internal/utils"
	"Koukyo_discord_bot/internal/version"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	_ "time/tzdata"

	"github.com/bwmarrin/discordgo"
)

// Global monitor instance
var globalMonitor *monitor.Monitor

func main() {
	cfg := config.Load()
	if cfg == nil {
		log.Fatal("Failed to load configuration")
	}
	if cfg.Token == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}

	// Bot情報の初期化
	botInfo := models.NewBotInfo(version.Version)

	// 設定マネージャーの初期化
	// Dockerコンテナ内では /app/data に保存
	settingsPath := filepath.Join(".", "data", "settings.json")
	if _, err := os.Stat("/app"); err == nil {
		// Dockerコンテナ内
		settingsPath = "/app/data/settings.json"
	}
	settingsManager := config.NewSettingsManager(settingsPath)
	defer settingsManager.Close() // ここを追加
	log.Printf("Settings loaded from: %s", settingsPath)
	dataDir := filepath.Dir(settingsPath)

	// レートリミッターの初期化
	limiter := utils.NewRateLimiter(2)
	defer limiter.Close()
	activityLimiter := utils.NewRateLimiter(2)
	defer activityLimiter.Close()

	// ユーザー活動トラッカーの初期化
	activityTracker := activity.NewTracker(activity.Config{
		TopLeftTileX:  1818,
		TopLeftTileY:  806,
		TopLeftPixelX: 989,
		TopLeftPixelY: 358,
		Width:         107,
		Height:        142,
	}, activityLimiter, dataDir)
	activityTracker.Start()
	defer activityTracker.Stop()

	// WebSocket監視の開始
	if cfg.WebSocketURL != "" {
		globalMonitor = monitor.NewMonitor(cfg.WebSocketURL)
		globalMonitor.SetActivityTracker(activityTracker)

		if err := globalMonitor.Start(); err != nil {
			log.Printf("Failed to start monitor: %v", err)
			log.Println("Continuing without monitor...")
		} else {
			log.Printf("Monitor started: %s", cfg.WebSocketURL)
		}
	} else {
		log.Println("WEBSOCKET_URL not set, skipping monitor")
	}

	dg, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		log.Printf("Failed to create Discord session: %v", err)
		return
	}
	// Prevent rare but catastrophic "stuck REST call" situations from blocking the
	// monitoring loop indefinitely (e.g. small-diff edits).
	if dg.Client != nil {
		dg.Client.Timeout = 15 * time.Second
	}

	// Intentsを設定
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent | discordgo.IntentsGuilds

	// 通知システムの初期化
	var notifier *notifications.Notifier
	if globalMonitor != nil {
		notifier = notifications.NewNotifier(dg, globalMonitor, settingsManager, dataDir)
		notifier.StartMonitoring()
		log.Println("Notification system started")
	}
	if notifier != nil {
		activityTracker.SetNewUserCallback(notifier.NotifyNewUser)
	}

	h := handler.NewHandler("!", botInfo, globalMonitor, settingsManager, notifier, limiter, activityLimiter, dataDir) // settingsManager を渡す
	dg.AddHandler(h.OnReady)
	dg.AddHandler(h.OnResumed)
	dg.AddHandler(h.OnMessage)
	dg.AddHandler(h.OnInteractionCreate)

	err = dg.Open()
	if err != nil {
		log.Printf("Failed to open Discord session: %v", err)
		return
	}

	log.Printf("Bot started - Version: %s, Date: %s\n", version.Version, time.Now().Format("2006-01-02"))
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	sig := <-sigCh
	signal.Stop(sigCh)
	log.Printf("Shutdown signal received: %s", sig.String())

	shutdownDone := make(chan struct{})
	go func() {
		h.Cleanup(dg)
		if globalMonitor != nil {
			globalMonitor.Stop()
		}
		dg.Close()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
		log.Println("Shutdown complete")
	case <-time.After(10 * time.Second):
		log.Println("Shutdown timed out after 10s, forcing exit")
	}
}
