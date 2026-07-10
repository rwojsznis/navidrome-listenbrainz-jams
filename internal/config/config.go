// Package config loads and validates the service configuration from a YAML
// file. String values support ${ENV} / ${ENV:-default} interpolation so secrets
// can be supplied via environment variables instead of being stored in plaintext.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration loaded from the mounted YAML file.
type Config struct {
	PollInterval time.Duration `yaml:"poll_interval"`
	Navidrome    Navidrome     `yaml:"navidrome"`
	Slskd        Slskd         `yaml:"slskd"`
	Paths        Paths         `yaml:"paths"`
	Download     Download      `yaml:"download"`
	Matching     Matching      `yaml:"matching"`
	Fingerprint  Fingerprint   `yaml:"fingerprint"`
	Lyrics       Lyrics        `yaml:"lyrics"`
	Ytdlp        Ytdlp         `yaml:"ytdlp"`
	State        State         `yaml:"state"`
	Web          Web           `yaml:"web"`
	Feeds        []Feed        `yaml:"feeds"`
}

// Fingerprint controls the optional acoustic-fingerprinting step that identifies
// a freshly downloaded file via Chromaprint/AcoustID and writes its MusicBrainz
// recording id into the file's tags (so Navidrome indexes it). Disabled by
// default; it requires the external `fpcalc` and `opustags` binaries.
type Fingerprint struct {
	// Enabled turns the step on. When false, downloads are imported untagged.
	Enabled bool `yaml:"enabled"`
	// AcoustIDAPIKey is the free AcoustID *application* API key (the "client" key
	// for lookups), from https://acoustid.org/new-application. NOT the user/account
	// key, which is only for submitting fingerprints and is rejected on lookup.
	// Required when enabled.
	AcoustIDAPIKey string `yaml:"acoustid_api_key"`
	// FpcalcPath is the Chromaprint fpcalc binary (default "fpcalc" via PATH).
	FpcalcPath string `yaml:"fpcalc_path"`
	// OpustagsPath is the opustags binary used to tag .opus files (default
	// "opustags" via PATH). FLAC and MP3 are tagged in pure Go.
	OpustagsPath string `yaml:"opustags_path"`
}

// Lyrics controls the optional lyrics-fetching step. When enabled, each freshly
// imported file gets a sibling ".lrc" written from lrclib.net (synced lyrics
// preferred, plain as fallback) — unless a ".lrc" already exists. Disabled by
// default; uses only the artist + title the feed already provides.
type Lyrics struct {
	// Enabled turns the step on.
	Enabled bool `yaml:"enabled"`
	// LrclibURL is the lrclib API base URL (default "https://lrclib.net").
	LrclibURL string `yaml:"lrclib_url"`
}

// Ytdlp controls the optional yt-dlp fallback download source. It is tried only
// after slskd has exhausted its retries for a track: yt-dlp searches YouTube,
// downloads the top hit's audio, and feeds it through the same import path as
// slskd downloads. Disabled by default; it requires the external `yt-dlp` and
// `ffmpeg` binaries (and a JS runtime, `deno`, for YouTube signature
// descrambling) — all bundled in the Docker image.
type Ytdlp struct {
	// Enabled turns the fallback on. When false, a track slskd can't find stays
	// missing (as before).
	Enabled bool `yaml:"enabled"`
	// BinaryPath is the yt-dlp binary (default "yt-dlp" via PATH).
	BinaryPath string `yaml:"binary_path"`
	// AudioFormat is the target audio format passed to `yt-dlp -x --audio-format`.
	// YouTube audio is lossy (opus/aac), so a lossy container ("mp3", default, or
	// "opus") is right; transcoding to FLAC only bloats a lossy stream.
	AudioFormat string `yaml:"audio_format"`
	// MaxDuration bounds an accepted result's length (converted to seconds in
	// yt-dlp's --match-filter), rejecting hour-long album/live uploads. Default 10m.
	MaxDuration time.Duration `yaml:"max_duration"`
	// Timeout caps a single yt-dlp invocation (search + download). Default 5m.
	Timeout time.Duration `yaml:"timeout"`
	// CookiesFile is an optional Netscape-format cookies file passed to
	// `yt-dlp --cookies` for age-restricted or region-locked content.
	CookiesFile string `yaml:"cookies_file"`
}

// Web controls the read-only status dashboard.
type Web struct {
	// Listen is the address to serve the UI on (e.g. ":8080"). Empty disables it.
	Listen string `yaml:"listen"`
}

// Navidrome holds connection details for the Navidrome (Subsonic) instance.
type Navidrome struct {
	URL string `yaml:"url"`
	// AdminUser / AdminPass are optional credentials used for operations that
	// require an admin-capable user — currently triggering library rescans
	// (Subsonic startScan, which returns "not authorized" for regular users).
	// When omitted, the first feed's credentials are used (which then must be
	// admin). Set these when your feed users are not admins.
	AdminUser string `yaml:"admin_user"`
	AdminPass string `yaml:"admin_pass"`
}

// ScanUser returns the credentials to use for admin-only operations such as
// library rescans: the dedicated admin credentials when configured, otherwise
// the first feed's credentials as a fallback.
func (c *Config) ScanUser() (user, pass string) {
	if c.Navidrome.AdminUser != "" {
		return c.Navidrome.AdminUser, c.Navidrome.AdminPass
	}
	if len(c.Feeds) > 0 {
		return c.Feeds[0].NavidromeUser, c.Feeds[0].NavidromePass
	}
	return "", ""
}

// Slskd holds connection details for the slskd instance.
type Slskd struct {
	URL    string `yaml:"url"`
	APIKey string `yaml:"api_key"`
}

// Paths describes the filesystem locations the service mounts and moves between.
type Paths struct {
	// SlskdDownloads is slskd's completed-downloads directory (read-only).
	SlskdDownloads string `yaml:"slskd_downloads"`
	// ImportDir is the directory where imported files are written (read-write).
	// It must sit inside Navidrome's music library so Navidrome indexes them.
	// Mount ONLY this directory into the container, not the whole library.
	ImportDir string `yaml:"import_dir"`
}

// Download controls how candidate files are selected and retried on slskd.
type Download struct {
	FormatPreference []string      `yaml:"format_preference"`
	MinBitrate       int           `yaml:"min_bitrate"`
	PerTrackTimeout  time.Duration `yaml:"per_track_timeout"`
	MaxRetries       int           `yaml:"max_retries"`
}

// Matching controls fuzzy matching of search results to feed tracks.
type Matching struct {
	FuzzyThreshold float64 `yaml:"fuzzy_threshold"`
}

// State controls the persistent state store.
type State struct {
	DBPath string `yaml:"db_path"`
}

// Feed is a single ListenBrainz RSS source mapped to a Navidrome user.
type Feed struct {
	Name          string `yaml:"name"`
	RSSURL        string `yaml:"rss_url"`
	NavidromeUser string `yaml:"navidrome_user"`
	NavidromePass string `yaml:"navidrome_pass"`
}

// Default values applied when fields are omitted from the YAML.
var defaults = Config{
	PollInterval: 30 * time.Minute,
	Paths:        Paths{ImportDir: "/import"},
	Download: Download{
		FormatPreference: []string{"flac", "mp3"},
		MinBitrate:       256,
		PerTrackTimeout:  2 * time.Hour,
		MaxRetries:       3,
	},
	Matching:    Matching{FuzzyThreshold: 0.85},
	Fingerprint: Fingerprint{FpcalcPath: "fpcalc", OpustagsPath: "opustags"},
	Lyrics:      Lyrics{LrclibURL: "https://lrclib.net"},
	Ytdlp: Ytdlp{
		BinaryPath:  "yt-dlp",
		AudioFormat: "mp3",
		MaxDuration: 10 * time.Minute,
		Timeout:     5 * time.Minute,
	},
	State: State{DBPath: "/data/state.db"},
	Web:   Web{Listen: ":8080"},
}

// Load reads, interpolates, parses and validates the config file at path.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	interpolated, err := interpolate(string(raw))
	if err != nil {
		return nil, err
	}

	cfg := defaults
	if err := yaml.Unmarshal([]byte(interpolated), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// envPattern matches ${NAME} and ${NAME:-default}.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// interpolate replaces ${ENV} / ${ENV:-default} references with environment
// values. An unset variable with no default is an error so misconfiguration
// fails loudly instead of silently injecting an empty secret.
func interpolate(s string) (string, error) {
	var missing []string
	out := envPattern.ReplaceAllStringFunc(s, func(match string) string {
		groups := envPattern.FindStringSubmatch(match)
		name := groups[1]
		hasDefault := groups[2] != ""
		def := groups[3]
		if val, ok := os.LookupEnv(name); ok {
			return val
		}
		if hasDefault {
			return def
		}
		missing = append(missing, name)
		return match
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("config references unset environment variables: %v", missing)
	}
	return out, nil
}

func (c *Config) validate() error {
	if c.Navidrome.URL == "" {
		return fmt.Errorf("navidrome.url is required")
	}
	if c.Slskd.URL == "" {
		return fmt.Errorf("slskd.url is required")
	}
	if c.Slskd.APIKey == "" {
		return fmt.Errorf("slskd.api_key is required")
	}
	if c.Paths.SlskdDownloads == "" {
		return fmt.Errorf("paths.slskd_downloads is required")
	}
	if c.Paths.ImportDir == "" {
		return fmt.Errorf("paths.import_dir is required")
	}
	if c.Fingerprint.Enabled && c.Fingerprint.AcoustIDAPIKey == "" {
		return fmt.Errorf("fingerprint.acoustid_api_key is required when fingerprint.enabled is true")
	}
	if c.Ytdlp.Enabled {
		switch c.Ytdlp.AudioFormat {
		case "mp3", "opus", "aac", "m4a", "vorbis", "flac", "wav", "best":
		case "":
			return fmt.Errorf("ytdlp.audio_format is required when ytdlp.enabled is true")
		default:
			return fmt.Errorf("ytdlp.audio_format %q is not a valid yt-dlp --audio-format", c.Ytdlp.AudioFormat)
		}
	}
	if len(c.Feeds) == 0 {
		return fmt.Errorf("at least one feed is required")
	}
	seen := make(map[string]bool)
	for i, f := range c.Feeds {
		if f.Name == "" {
			return fmt.Errorf("feeds[%d].name is required", i)
		}
		if seen[f.Name] {
			return fmt.Errorf("duplicate feed name %q", f.Name)
		}
		seen[f.Name] = true
		if f.RSSURL == "" {
			return fmt.Errorf("feeds[%d] (%s): rss_url is required", i, f.Name)
		}
		if f.NavidromeUser == "" {
			return fmt.Errorf("feeds[%d] (%s): navidrome_user is required", i, f.Name)
		}
		if f.NavidromePass == "" {
			return fmt.Errorf("feeds[%d] (%s): navidrome_pass is required", i, f.Name)
		}
	}
	return nil
}
