// Command navidrome-lb-jams runs the ListenBrainz -> Navidrome playlist
// assembly daemon.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/downloader"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/files"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/fingerprint"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/listenbrainz"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/lyrics"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/navidrome"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/pipeline"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/slskd"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/tags"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/web"
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

	// Library rescans (Subsonic startScan) require an admin-capable user: the
	// dedicated navidrome.admin_user/admin_pass when set, else the first feed's
	// credentials (which then must belong to an admin).
	scanUser, scanPass := cfg.ScanUser()
	scanClient := navidrome.New(cfg.Navidrome.URL, scanUser, scanPass)
	pipe := pipeline.New(st, cfg, logger)
	dl := downloader.New(slskd.New(cfg.Slskd.URL, cfg.Slskd.APIKey), scanClient, cfg, logger)
	var tagger *fingerprint.Service
	if cfg.Fingerprint.Enabled {
		tagger = fingerprint.New(cfg, logger)
		dl.SetTagger(tagger)
		slog.Info("acoustic fingerprinting enabled")
	}
	var rescanLyrics web.RescanLyricsFunc
	if cfg.Lyrics.Enabled {
		lyricsSvc := lyrics.New(cfg.Lyrics.LrclibURL, logger)
		dl.SetLyrics(lyricsSvc)
		slog.Info("lyrics fetching enabled", "source", cfg.Lyrics.LrclibURL)

		// Manual "re-scan lyrics" action: fetch a sibling .lrc for every imported
		// track in a playlist that doesn't already have one, recording the result.
		// Runs in the web goroutine; SetLyricsStatus touches only the lyrics column
		// so it can't clobber concurrent pipeline progress on the same track.
		rescanLyrics = func(ctx context.Context, playlistID int64) (int, error) {
			tracks, err := st.TracksFor(playlistID)
			if err != nil {
				return 0, err
			}
			processed := 0
			for i := range tracks {
				t := &tracks[i]
				if t.ImportedPath == "" {
					continue // nothing on disk to attach lyrics to yet
				}
				status, err := lyricsSvc.WriteAlongside(ctx, t.ImportedPath, t.Artist, t.Title)
				if err != nil {
					logger.Warn("rescan lyrics", "track", t.Title, "err", err)
					continue
				}
				if status != t.LyricsStatus {
					if err := st.SetLyricsStatus(t.ID, status); err != nil {
						logger.Warn("save lyrics status", "track", t.Title, "err", err)
					}
				}
				processed++
			}
			return processed, nil
		}
	}
	pipe.SetDownloader(dl)

	// Manual "tag MBID" action for a track stuck in "downloaded" (imported but
	// not matched in Navidrome). We downloaded the file FOR a specific feed
	// entry, so the feed's recording MBID is authoritative: write it straight
	// into the file's tags (no AcoustID round-trip) and rescan, so resolve() can
	// match by MBID. Falls back to AcoustID only when the feed carries no MBID.
	// Runs in the web goroutine; safe because the tag writer/scan client are
	// stateless and the daemon never writes a downloaded track's file.
	tagWriter := tags.Writer{OpustagsPath: cfg.Fingerprint.OpustagsPath}
	retag := func(ctx context.Context, trackID int64) (int64, error) {
		t, err := st.TrackByID(trackID)
		if err != nil || t == nil {
			return 0, err
		}
		if t.ImportedPath == "" {
			return t.PlaylistID, nil // nothing on disk to tag
		}
		var terr error
		switch {
		case t.RecordingMBID != "":
			terr = tagWriter.WriteRecordingMBID(ctx, t.ImportedPath, t.RecordingMBID)
		case tagger != nil:
			terr = tagger.Tag(ctx, t.ImportedPath, "")
		default:
			terr = fmt.Errorf("no feed MBID and fingerprinting disabled")
		}
		t.LastError = ""
		if terr != nil {
			t.LastError = terr.Error()
		}
		_ = st.UpdateTrack(t)
		if terr != nil {
			return t.PlaylistID, terr
		}
		if serr := scanClient.StartScan(ctx); serr != nil {
			logger.Warn("retag rescan", "err", serr)
		}
		return t.PlaylistID, nil
	}

	// Manual "delete & restart" action for a track stuck in "downloaded" with the
	// wrong file (slskd returned the closest, not the correct, recording). Delete
	// the imported file (and its sibling .lrc) and reset the track so the daemon
	// searches again — now gated by the stricter slskd acceptance check, so it
	// won't re-import the same wrong file.
	discard := func(ctx context.Context, trackID int64) (int64, error) {
		t, err := st.TrackByID(trackID)
		if err != nil || t == nil {
			return 0, err
		}
		if t.ImportedPath != "" {
			if rerr := files.Remove(t.ImportedPath); rerr != nil {
				logger.Warn("discard remove file", "track", t.Title, "path", t.ImportedPath, "err", rerr)
			}
		}
		return st.RetryTrack(trackID)
	}

	app := &app{
		cfg:   cfg,
		store: st,
		lb:    listenbrainz.NewClient(),
		pipe:  pipe,
	}

	if *once {
		app.tick(ctx)
		return
	}

	// Read-only status dashboard (daemon mode only).
	if cfg.Web.Listen != "" {
		websrv := web.New(st, cfg.Web.Listen, logger, retag, rescanLyrics, discard)
		go func() {
			slog.Info("dashboard listening", "addr", cfg.Web.Listen)
			if err := websrv.Start(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("dashboard server", "err", err)
			}
		}()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = websrv.Shutdown(shutCtx)
		}()
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
	cfg   *config.Config
	store *store.Store
	lb    *listenbrainz.Client
	pipe  *pipeline.Pipeline
}

// tick performs one pass: discover new playlists from feeds and (later) advance
// in-progress ones through the assembly state machine.
func (a *app) tick(ctx context.Context) {
	slog.Info("tick: start")
	a.discover(ctx)
	a.pipe.Run(ctx)
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
