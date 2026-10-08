package config

import "os"

type Config struct {
	Token string
}

func Load() *Config {
	return &Config{
		Token: os.Getenv("DISCORD_TOKEN"),
	}
}
