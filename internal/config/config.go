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
	Host           string   `mapstructure:"host"`
	Port           int      `mapstructure:"port"`
	BaseURL        string   `mapstructure:"base_url"`
	AllowedOrigins []string `mapstructure:"allowed_origins"`
}

type ATProtoConfig struct {
	PDSURL   string `mapstructure:"pds_url"`
	Handle   string `mapstructure:"handle"`
	Password string `mapstructure:"password"`
	UseDPoP  bool   `mapstructure:"use_dpop"`

	// PLCDirectoryURL is the did:plc directory used to resolve did:plc
	// identities to their PDS (see internal/atproto.ResolvePDS /
	// atchess-1c9.10). Defaults to the real, public https://plc.directory.
	// The local dual-PDS test harness overrides this (ATPROTO_PLC_DIRECTORY_URL)
	// to point at its own hermetic did:plc server, since the harness's
	// accounts do not exist on the public directory.
	PLCDirectoryURL string `mapstructure:"plc_directory_url"`
}

type DevelopmentConfig struct {
	Debug    bool   `mapstructure:"debug"`
	LogLevel string `mapstructure:"log_level"`
}

type FirehoseConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// URL is one or more (comma-separated, see SplitFirehoseURLs)
	// com.atproto.sync.subscribeRepos websocket endpoints to subscribe to,
	// AND (atchess-1c9.46) the closed set of PDS hosts the login-time
	// repo-read backfill (internal/backfill) is allowed to enumerate --
	// see that package's doc comment for why it is bounded to exactly this
	// list rather than attempting to search the whole network.
	//
	// For a real single-PDS deployment (e.g. testing against one real
	// bsky.social account), this should point at that PDS's own
	// subscribeRepos endpoint, or -- once a genuine network-wide relay is
	// in front of it -- wss://bsky.network/xrpc/com.atproto.sync.subscribeRepos
	// (the real, public AT Protocol relay; NOT bsky.social, which is a
	// single very large PDS, not a relay). See docs/firehose-and-backfill.md.
	//
	// The comma-separated multi-URL form exists for test topologies with
	// more than one PDS and no relay in front of them (this project's local
	// dual-PDS harness, see test/harness/services.go, which lists its own
	// known PDSes directly since no relay exists there) -- it is not
	// expected to be used in a real deployment against the public network.
	URL string `mapstructure:"url"`

	// StateDir is the directory used to persist each watched host's last
	// processed firehose sequence number across process restarts
	// (atchess-1c9.46; see internal/firehose.CursorStore), so a restart
	// resumes from where it left off instead of either replaying a host's
	// entire retained commit log (the defect this field's addition fixes)
	// or silently discarding history by always starting at the live tip.
	// This project has no database (see CLAUDE.md), so a small file under
	// this directory is the natural fit. Must be writable by the process;
	// created if it does not exist.
	StateDir string `mapstructure:"state_dir"`

	// Transport optionally forces which wire protocol every
	// firehose.Client built from URL speaks, overriding the automatic
	// per-URL guess (firehose.DetectTransport: a URL whose path ends in
	// "/subscribe" is treated as Jetstream's JSON transport, everything
	// else -- notably a com.atproto.sync.subscribeRepos XRPC path -- as
	// the original CBOR transport). Recognized values: "subscribeRepos"
	// (or "cbor") and "jetstream". Empty (the default) means "guess per
	// URL", which is unchanged behavior for every existing deployment's
	// configured URL (see atchess-1c9.49).
	//
	// Jetstream (https://github.com/bluesky-social/jetstream) is a public
	// service that filters com.atproto.sync.subscribeRepos server-side by
	// collection NSID (wantedCollections) and re-emits matching commits as
	// JSON, so a self-hosted deployment on a small VPS does not have to
	// decode and discard the entire Bluesky network firehose in CBOR just
	// to find the near-zero fraction of it that is app.atchess.* --
	// see docs/firehose-and-backfill.md and internal/firehose/README.md.
	Transport string `mapstructure:"transport"`
}

// SplitFirehoseURLs parses raw (see FirehoseConfig.URL's doc comment) as a
// comma-separated list of com.atproto.sync.subscribeRepos websocket URLs,
// trimming whitespace and dropping empty entries. Shared by
// cmd/protocol/main.go (which subscribes to each one) and internal/web
// (which bounds the login-time repo-read backfill to the same closed list)
// so the two "which PDSes does this deployment know about" answers can
// never drift apart. Lives here, in the leaf config package, rather than in
// internal/firehose or internal/backfill, specifically to avoid an import
// cycle: internal/firehose imports internal/web (EventProcessor uses
// web.Hub), so internal/web cannot import internal/firehose, and
// internal/backfill must be importable from internal/web without also
// being reachable from internal/firehose.
func SplitFirehoseURLs(raw string) []string {
	var urls []string
	for _, part := range strings.Split(raw, ",") {
		u := strings.TrimSpace(part)
		if u != "" {
			urls = append(urls, u)
		}
	}
	return urls
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
	viper.BindEnv("server.allowed_origins", "SERVER_ALLOWED_ORIGINS", "ATCHESS_SERVER_ALLOWED_ORIGINS")
	viper.BindEnv("atproto.pds_url", "ATPROTO_PDS_URL", "ATCHESS_ATPROTO_PDS_URL")
	viper.BindEnv("atproto.handle", "ATPROTO_HANDLE", "ATCHESS_ATPROTO_HANDLE")
	viper.BindEnv("atproto.password", "ATPROTO_PASSWORD", "ATCHESS_ATPROTO_PASSWORD")
	viper.BindEnv("atproto.use_dpop", "ATPROTO_USE_DPOP", "ATCHESS_ATPROTO_USE_DPOP")
	viper.BindEnv("atproto.plc_directory_url", "ATPROTO_PLC_DIRECTORY_URL", "ATCHESS_ATPROTO_PLC_DIRECTORY_URL")
	viper.BindEnv("development.debug", "DEVELOPMENT_DEBUG", "ATCHESS_DEVELOPMENT_DEBUG")
	viper.BindEnv("development.log_level", "DEVELOPMENT_LOG_LEVEL", "ATCHESS_DEVELOPMENT_LOG_LEVEL")
	viper.BindEnv("firehose.enabled", "FIREHOSE_ENABLED", "ATCHESS_FIREHOSE_ENABLED")
	viper.BindEnv("firehose.url", "FIREHOSE_URL", "ATCHESS_FIREHOSE_URL")
	viper.BindEnv("firehose.state_dir", "FIREHOSE_STATE_DIR", "ATCHESS_FIREHOSE_STATE_DIR")
	viper.BindEnv("firehose.transport", "FIREHOSE_TRANSPORT", "ATCHESS_FIREHOSE_TRANSPORT")

	// Set defaults
	viper.SetDefault("server.host", "localhost")
	viper.SetDefault("server.port", 8080)
	viper.SetDefault("atproto.pds_url", "http://localhost:3000")
	viper.SetDefault("atproto.use_dpop", false)
	viper.SetDefault("atproto.plc_directory_url", "https://plc.directory")
	viper.SetDefault("development.debug", false)
	viper.SetDefault("development.log_level", "info")
	viper.SetDefault("firehose.enabled", false)
	viper.SetDefault("firehose.url", "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos")
	viper.SetDefault("firehose.state_dir", "./data/firehose")
	viper.SetDefault("firehose.transport", "")

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
			PDSURL:          "http://localhost:3000",
			PLCDirectoryURL: "https://plc.directory",
		},
		Development: DevelopmentConfig{
			Debug:    false,
			LogLevel: "info",
		},
		Firehose: FirehoseConfig{
			Enabled:   false,
			URL:       "wss://bsky.social/xrpc/com.atproto.sync.subscribeRepos",
			StateDir:  "./data/firehose",
			Transport: "",
		},
	}
}
