package handler

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"Koukyo_discord_bot/internal/eastereggs"

	"github.com/bwmarrin/discordgo"
)

const easterEggCacheTTL = 1 * time.Minute

type easterEggCache struct {
	mu        sync.Mutex
	eggs      map[string]eggConfig
	aliases   map[string]eggConfig
	fetchedAt time.Time
}

var globalEasterEggCache easterEggCache

func (h *Handler) handleEasterEgg(s *discordgo.Session, m *discordgo.MessageCreate, cmdName string) bool {
	if reply, ok := eastereggs.HandleSleepyHeresy(m.Content, m.GuildID, m.Author.ID); ok {
		return sendEasterEggReply(s, m.ChannelID, reply)
	}
	if replies, ok := eastereggs.HandleSleepyboard(cmdName, m.GuildID, m.Author.ID); ok {
		for _, reply := range replies {
			sendEasterEggReply(s, m.ChannelID, reply)
		}
		return true
	}
	if h.dataDir == "" {
		return false
	}
	cfg, ok := loadEasterEggConfig(h.dataDir, cmdName)
	if !ok || (cfg.Reply == "" && len(cfg.Choices) == 0) {
		return false
	}

	if !eastereggs.ShouldTriggerChance(cfg.ChancePercent) {
		return false
	}

	reply := cfg.Reply
	if len(cfg.Choices) > 0 {
		chosen, ok := eastereggs.WeightedChoice(cfg.Choices)
		if !ok || chosen == "" {
			return false
		}
		reply = chosen
	}

	return sendEasterEggReply(s, m.ChannelID, reply)
}

type eggConfig struct {
	Reply         string
	ChancePercent float64
	Choices       []eastereggs.WeightedReply
	Aliases       []string
}

func (c *eggConfig) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Reply = s
		c.ChancePercent = 100
		return nil
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.ChancePercent = 100
	if v, ok := raw["reply"]; ok {
		_ = json.Unmarshal(v, &c.Reply)
	}
	if v, ok := raw["chance"]; ok {
		_ = json.Unmarshal(v, &c.ChancePercent)
	}
	if v, ok := raw["choices"]; ok {
		_ = json.Unmarshal(v, &c.Choices)
	}
	if v, ok := raw["aliases"]; ok {
		_ = json.Unmarshal(v, &c.Aliases)
	}
	return nil
}

func sendEasterEggReply(s *discordgo.Session, channelID, reply string) bool {
	if _, err := s.ChannelMessageSend(channelID, reply); err != nil {
		log.Printf("Failed to send easter egg response: %v", err)
		return false
	}
	return true
}

func loadEasterEggConfig(dataDir, cmdName string) (eggConfig, bool) {
	eggs, aliases, ok := loadEasterEggConfigs(dataDir)
	if !ok {
		return eggConfig{}, false
	}

	loweredCmdName := strings.ToLower(cmdName)
	if cfg, exists := eggs[loweredCmdName]; exists {
		return cfg, true
	}
	cfg, exists := aliases[loweredCmdName]
	return cfg, exists
}

func loadEasterEggConfigs(dataDir string) (map[string]eggConfig, map[string]eggConfig, bool) {
	globalEasterEggCache.mu.Lock()
	defer globalEasterEggCache.mu.Unlock()

	if globalEasterEggCache.eggs != nil &&
		time.Since(globalEasterEggCache.fetchedAt) < easterEggCacheTTL {
		return globalEasterEggCache.eggs, globalEasterEggCache.aliases, true
	}

	path := filepath.Join(dataDir, "easter_eggs.json")
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil, nil, false
	}

	raw := make(map[string]json.RawMessage)
	if err := json.Unmarshal(data, &raw); err != nil {
		log.Printf("Failed to parse easter_eggs.json: %v", err)
		return nil, nil, false
	}

	eggs := make(map[string]eggConfig, len(raw))
	aliases := make(map[string]eggConfig)
	for key, blob := range raw {
		cfg := eggConfig{}
		if err := cfg.UnmarshalJSON(blob); err != nil {
			log.Printf("Failed to parse easter_eggs.json entry %s: %v", key, err)
			continue
		}
		normalizedKey := strings.ToLower(key)
		eggs[normalizedKey] = cfg
		for _, alias := range cfg.Aliases {
			aliases[strings.ToLower(alias)] = cfg
		}
	}

	if len(eggs) == 0 {
		return nil, nil, false
	}

	globalEasterEggCache.eggs = eggs
	globalEasterEggCache.aliases = aliases
	globalEasterEggCache.fetchedAt = time.Now()
	return eggs, aliases, true
}
