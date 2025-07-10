package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	ATProto     ATProtoConfig     `mapstructure:"atproto"`
	Development DevelopmentConfig `mapstructure:"development"`
}

type ServerConfig struct {
	Host string `mapstructure:"host"`
	Port int    `mapstructure:"port"`
}

type ATProtoConfig struct {
	PDSURL   string `mapstructure:"pds_url"`
	Handle   string `mapstructure:"handle"`
	Password string `mapstructure:"password"`
}

type DevelopmentConfig struct {
	Debug    bool   `mapstructure:"debug"`
	LogLevel string `mapstructure:"log_level"`
}

func Load() (*Config, error) {
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
	viper.AddConfigPath("./config")
	
	// Enable environment variables
	viper.SetEnvPrefix("ATCHESS")
	viper.AutomaticEnv()
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	
	// Set defaults
	viper.SetDefault("server.host", "localhost")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("atproto.pds_url", "http://localhost:3000")
	viper.SetDefault("development.debug", false)
	viper.SetDefault("development.log_level", "info")
	
	// Read config
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); ok {
			// Config file not found, use defaults
			return loadDefaults(), nil
		}
		return nil, fmt.Errorf("failed to read config: %w", err)
	}
	
	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}
	
	return &cfg, nil
}

func loadDefaults() *Config {
	return &Config{
		Server: ServerConfig{
			Host: "localhost",
			Port: 8080,
		},
		ATProto: ATProtoConfig{
			PDSURL: "http://localhost:3000",
		},
		Development: DevelopmentConfig{
			Debug:    false,
			LogLevel: "info",
		},
	}
}