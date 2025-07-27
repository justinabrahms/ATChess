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
	Firehose    FirehoseConfig    `mapstructure:"firehose"`
}

type ServerConfig struct {
	Host    string `mapstructure:"host"`
	Port    int    `mapstructure:"port"`
	BaseURL string `mapstructure:"base_url"`
}

type ATProtoConfig struct {
	PDSURL    string `mapstructure:"pds_url"`
	Handle    string `mapstructure:"handle"`
	Password  string `mapstructure:"password"`
	UseDPoP   bool   `mapstructure:"use_dpop"`
}

type DevelopmentConfig struct {
	Debug    bool   `mapstructure:"debug"`
	LogLevel string `mapstructure:"log_level"`
}

type FirehoseConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	URL     string `mapstructure:"url"`
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
	
	// Also bind specific environment variables for compatibility
	// This allows both ATCHESS_ prefixed and unprefixed versions
	viper.BindEnv("server.host", "SERVER_HOST", "ATCHESS_SERVER_HOST")
	viper.BindEnv("server.port", "SERVER_PORT", "ATCHESS_SERVER_PORT")
	viper.BindEnv("server.base_url", "SERVER_BASE_URL", "ATCHESS_SERVER_BASE_URL")
	viper.BindEnv("atproto.pds_url", "ATPROTO_PDS_URL", "ATCHESS_ATPROTO_PDS_URL")
	viper.BindEnv("atproto.handle", "ATPROTO_HANDLE", "ATCHESS_ATPROTO_HANDLE")
	viper.BindEnv("atproto.password", "ATPROTO_PASSWORD", "ATCHESS_ATPROTO_PASSWORD")
	viper.BindEnv("atproto.use_dpop", "ATPROTO_USE_DPOP", "ATCHESS_ATPROTO_USE_DPOP")
	viper.BindEnv("development.debug", "DEVELOPMENT_DEBUG", "ATCHESS_DEVELOPMENT_DEBUG")
	viper.BindEnv("development.log_level", "DEVELOPMENT_LOG_LEVEL", "ATCHESS_DEVELOPMENT_LOG_LEVEL")
	viper.BindEnv("firehose.enabled", "FIREHOSE_ENABLED", "ATCHESS_FIREHOSE_ENABLED")
	viper.BindEnv("firehose.url", "FIREHOSE_URL", "ATCHESS_FIREHOSE_URL")
	
	// Set defaults
	viper.SetDefault("server.host", "localhost")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("atproto.pds_url", "http://localhost:3000")
	viper.SetDefault("atproto.use_dpop", false)
	viper.SetDefault("development.debug", false)
	viper.SetDefault("development.log_level", "info")
	viper.SetDefault("firehose.enabled", false)
	viper.SetDefault("firehose.url", "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos")
	
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
		Firehose: FirehoseConfig{
			Enabled: false,
			URL:     "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos",
		},
	}
}