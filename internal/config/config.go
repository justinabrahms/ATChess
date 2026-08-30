package config

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Server      ServerConfig      `mapstructure:"server"`
	ATProto     ATProtoConfig     `mapstructure:"atproto"`
	Development DevelopmentConfig `mapstructure:"development"`
	Firehose    FirehoseConfig    `mapstructure:"firehose"`
	Challenge   ChallengeConfig   `mapstructure:"challenge"`
	Session     SessionConfig     `mapstructure:"session"`
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

// ChallengeConfig configures the durable challenge index (atchess-1c9.50):
// a SQLite-backed AppView, keyed by challenged DID, that
// internal/firehose.EventProcessor and internal/backfill both write into
// and internal/web.Service.GetChallengeNotificationsHandler queries. See
// internal/challenge.Store's doc comment and docs/firehose-and-backfill.md
// for what this does and does not make discoverable.
type ChallengeConfig struct {
	// DBPath is the SQLite database file used to persist the challenge
	// index across restarts. Follows the same pattern as
	// FirehoseConfig.StateDir: this project has no separate database
	// server (see CLAUDE.md and the sizing note in atchess-1c9.50) --
	// modernc.org/sqlite (a pure-Go, cgo-free driver, required since this
	// project builds with CGO_ENABLED=0) reads and writes a single file on
	// the same box. The parent directory is created automatically if it
	// does not exist. Must be writable by the process.
	DBPath string `mapstructure:"db_path"`

	// PruneInterval controls how often cmd/protocol/main.go's background
	// loop calls Store.PruneExpired to delete expired OPEN challenge rows
	// (atchess-1c9.47 part 1 -- see PruneExpired's doc comment for why
	// only open rows, never declined/removed tombstones, are ever
	// deleted by it). Accepts any time.ParseDuration-compatible string
	// (e.g. "1h", "15m"); viper's default mapstructure decode hooks parse
	// it automatically.
	//
	// Defaulted to 1h (see SetDefault below): chess challenges are rare
	// and the default challenge expiry is 24h (see
	// challenge.FromChallengeRecord), so there is no correctness pressure
	// to prune often -- this only needs to run often enough to keep the
	// table bounded on a small (~2GB/1vCPU) droplet without adding
	// needless I/O for a table that is expected to stay tiny.
	PruneInterval time.Duration `mapstructure:"prune_interval"`
}

// SessionConfig controls where logged-in sessions are mirrored.
type SessionConfig struct {
	// StorePath is the file sessions are persisted to. Empty disables
	// persistence, which means every restart logs every user out — the
	// behaviour this config exists to end.
	StorePath string `mapstructure:"store_path"`
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
	viper.BindEnv("challenge.db_path", "CHALLENGE_DB_PATH", "ATCHESS_CHALLENGE_DB_PATH")
	viper.BindEnv("session.store_path", "SESSION_STORE_PATH", "ATCHESS_SESSION_STORE_PATH")
	viper.BindEnv("challenge.prune_interval", "CHALLENGE_PRUNE_INTERVAL", "ATCHESS_CHALLENGE_PRUNE_INTERVAL")

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
	viper.SetDefault("challenge.db_path", "./data/challenges.db")
	// Sessions are mirrored here so a restart does not log every user out.
	// Holds refresh tokens and DPoP private keys; written 0600.
	viper.SetDefault("session.store_path", "./data/sessions.json")
	viper.SetDefault("challenge.prune_interval", "1h")

	// Read config. A missing config file is a supported deployment mode
	// (see viper.SetDefault above and ATCHESS_*/unprefixed env var
	// bindings above) -- only a malformed/unreadable file is an error.
	// Either way, execution falls through to the same
	// bind-env/unmarshal/validate pipeline below, so env vars and
	// validate() are honoured identically whether or not a config file is
	// present (see atchess-1c9.64: previously the not-found branch
	// short-circuited to a hardcoded loadDefaults() that consulted no
	// viper state, silently ignoring every env var and skipping
	// validate()).
	if err := viper.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config: %w", err)
		}
	}

	var cfg Config
	if err := viper.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// validate rejects config values that decode successfully (so they never
// trip viper/mapstructure's own decode-hook errors -- see the comment on
// ChallengeConfig.PruneInterval and atchess-1c9.62) but are unusable by
// whatever consumes them.
//
// Chosen over clamping-to-default-with-a-warning to match how this
// package already treats bad config: an unparseable
// challenge.prune_interval (e.g. "banana") already fails Load() outright
// with a "failed to unmarshal config" error naming the offending key,
// rather than being silently replaced by the default. A value that
// parses but is non-positive should fail exactly the same loud way, for
// the same reason -- a clamp would hide the operator's typo instead of
// telling them about it, and would leave the fixed error path
// inconsistent depending on whether viper happened to notice the
// problem at decode time.
func validate(cfg *Config) error {
	if cfg.Challenge.PruneInterval <= 0 {
		return fmt.Errorf(
			"invalid config: 'challenge.prune_interval' must be a positive duration (e.g. \"1h\", \"15m\"), got %s -- a zero or negative interval would panic time.NewTicker in cmd/protocol/main.go's prune loop",
			cfg.Challenge.PruneInterval,
		)
	}
	return nil
}
