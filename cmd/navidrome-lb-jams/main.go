// Command navidrome-lb-jams runs the ListenBrainz -> Navidrome playlist
// assembly daemon.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/downloader"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/listenbrainz"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/pipeline"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/slskd"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to the config file")
	once := flag.Bool("once", false, "run a single tick and exit (instead of the daemon loop)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	st, err := store.Open(cfg.State.DBPath)
	if err != nil {
		slog.Error("open state store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pipe := pipeline.New(st, cfg, logger)
	dl := downloader.New(slskd.New(cfg.Slskd.URL, cfg.Slskd.APIKey), cfg, logger)
	pipe.SetDownloader(dl)

	app := &app{
		cfg:   cfg,
		store: st,
		lb:    listenbrainz.NewClient(),
		pipe:  pipe,
		dl:    dl,
		// A rescan is triggered after imports; Subsonic startScan requires an
		// admin-capable user, so we use the first feed's credentials.
		scanClient: navidrome.New(cfg.Navidrome.URL, cfg.Feeds[0].NavidromeUser, cfg.Feeds[0].NavidromePass),
	}

	if *once {
		app.tick(ctx)
		return
	}

	slog.Info("daemon started", "poll_interval", cfg.PollInterval.String(), "feeds", len(cfg.Feeds))
	app.tick(ctx) // run immediately, then on the interval
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			return
		case <-ticker.C:
			app.tick(ctx)
		}
	}
}

type app struct {
	cfg        *config.Config
	store      *store.Store
	lb         *listenbrainz.Client
	pipe       *pipeline.Pipeline
	dl         *downloader.Downloader
	scanClient *navidrome.Client
}

// tick performs one pass: discover new playlists from feeds and (later) advance
// in-progress ones through the assembly state machine.
func (a *app) tick(ctx context.Context) {
	slog.Info("tick: start")
	a.discover(ctx)
	a.pipe.Run(ctx)
	// One debounced rescan per tick if any download was imported, so the next
	// tick can resolve the newly-indexed tracks into their playlists.
	if a.dl.ConsumeScan() {
		if err := a.scanClient.StartScan(ctx); err != nil {
			slog.Error("trigger rescan", "err", err)
		} else {
			slog.Info("triggered navidrome rescan after imports")
		}
	}
	slog.Info("tick: done")
}

// discover fetches each feed and records new playlists/tracks in the store.
func (a *app) discover(ctx context.Context) {
	for _, feed := range a.cfg.Feeds {
		f, err := a.lb.Fetch(ctx, feed.RSSURL)
		if err != nil {
			slog.Error("fetch feed", "feed", feed.Name, "err", err)
			continue
		}
		for _, entry := range f.Entries {
			pl, err := a.store.UpsertPlaylist(feed.Name, entry.ID, entry.Title, feed.NavidromeUser)
			if err != nil {
				slog.Error("upsert playlist", "feed", feed.Name, "title", entry.Title, "err", err)
				continue
			}
			for _, tr := range entry.Tracks {
				if err := a.store.UpsertTrack(pl.ID, tr.Position, tr.RecordingMBID, tr.Artist, tr.Title); err != nil {
					slog.Error("upsert track", "playlist", pl.Title, "track", tr.Title, "err", err)
				}
			}
			slog.Info("discovered playlist",
				"feed", feed.Name, "title", entry.Title,
				"tracks", len(entry.Tracks), "status", pl.Status)
		}
	}
}
