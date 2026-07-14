// Package web serves a small read-only status dashboard: a list of playlists
// fetched from the feeds with their download progress, and a per-playlist page
// showing each track's state. It reads directly from the store and depends only
// on the standard library.
package web

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

// RetagFunc re-runs the acoustic fingerprint/tag step on an already-imported
// track and triggers a rescan, returning the track's playlist id (for the UI
// redirect). It is nil when fingerprinting is disabled.
type RetagFunc func(ctx context.Context, trackID int64) (playlistID int64, err error)

// RescanLyricsFunc fetches missing lyrics for every imported track in a
// playlist, persisting each track's lyrics status. It returns how many tracks
// it processed. It is nil when lyrics fetching is disabled. It may be slow (one
// network lookup per track without a local .lrc), so callers run it in the
// background.
type RescanLyricsFunc func(ctx context.Context, playlistID int64) (processed int, err error)

// DiscardFunc deletes a track's imported file (a wrong download) and resets the
// track so the daemon searches for it afresh, returning the track's playlist id
// for the UI redirect.
type DiscardFunc func(ctx context.Context, trackID int64) (playlistID int64, err error)

// ResortFunc rewrites the Navidrome playlist so its tracks are in feed order.
// Downloads complete out of order, so songs land in the playlist as they arrive;
// this resorts an already-assembled playlist on demand. It is nil when no
// Navidrome pipeline is wired in.
type ResortFunc func(ctx context.Context, playlistID int64) error

// Server is the dashboard HTTP server.
type Server struct {
	store        *store.Store
	log          *slog.Logger
	srv          *http.Server
	retag        RetagFunc
	rescanLyrics RescanLyricsFunc
	discard      DiscardFunc
	resort       ResortFunc
}

// New builds a Server listening on addr (e.g. ":8080"). retag may be nil to
// disable the per-track re-fingerprint action; rescanLyrics may be nil to
// disable the per-playlist lyrics re-scan action; discard may be nil to disable
// the per-track delete-and-restart action; resort may be nil to disable the
// per-playlist sort-to-feed-order action.
func New(st *store.Store, addr string, log *slog.Logger, retag RetagFunc, rescanLyrics RescanLyricsFunc, discard DiscardFunc, resort ResortFunc) *Server {
	s := &Server{store: st, log: log, retag: retag, rescanLyrics: rescanLyrics, discard: discard, resort: resort}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/playlist/{id}", s.handlePlaylist)
	mux.HandleFunc("POST /track/{id}/retry", s.handleRetryTrack)
	mux.HandleFunc("POST /track/{id}/refingerprint", s.handleRefingerprint)
	mux.HandleFunc("POST /track/{id}/discard", s.handleDiscard)
	mux.HandleFunc("POST /playlist/{id}/retry-missing", s.handleRetryMissing)
	mux.HandleFunc("POST /playlist/{id}/resync", s.handleResync)
	mux.HandleFunc("POST /playlist/{id}/rescan-lyrics", s.handleRescanLyrics)
	mux.HandleFunc("POST /playlist/{id}/resort", s.handleResort)
	s.srv = &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s
}

// Start runs the server until Shutdown is called; it blocks.
func (s *Server) Start() error { return s.srv.ListenAndServe() }

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error { return s.srv.Shutdown(ctx) }

// --- view models ---

type counts struct {
	Total, InPlaylist, Downloading, Downloaded, Exists, Missing, Pending int
}

func (c counts) Percent() int {
	if c.Total == 0 {
		return 0
	}
	return c.InPlaylist * 100 / c.Total
}

// Done counts tracks that are in the playlist or available (resolved).
func (c counts) Resolved() int { return c.InPlaylist + c.Exists + c.Downloaded }

type playlistRow struct {
	store.Playlist
	Counts counts
}

type trackRow struct {
	store.Track
}

func countTracks(tracks []store.Track) counts {
	var c counts
	c.Total = len(tracks)
	for _, t := range tracks {
		switch t.Status {
		case store.TrackInPlaylist:
			c.InPlaylist++
		case store.TrackDownloading:
			c.Downloading++
		case store.TrackDownloaded:
			c.Downloaded++
		case store.TrackExists:
			c.Exists++
		case store.TrackMissing:
			c.Missing++
		default:
			c.Pending++
		}
	}
	return c
}

// --- handlers ---

const perPage = 10

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	playlists, err := s.store.AllPlaylists()
	if err != nil {
		s.fail(w, err)
		return
	}

	// Playlists come newest-first. Paginate so the page shows only the most
	// recent perPage; the ?page= query selects the window (1-based).
	total := len(playlists)
	totalPages := (total + perPage - 1) / perPage
	if totalPages < 1 {
		totalPages = 1
	}
	page := 1
	if p, err := strconv.Atoi(r.URL.Query().Get("page")); err == nil && p > 1 {
		page = p
	}
	if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * perPage
	end := start + perPage
	if end > total {
		end = total
	}
	pagePlaylists := playlists[start:end]

	rows := make([]playlistRow, 0, len(pagePlaylists))
	for _, p := range pagePlaylists {
		tracks, err := s.store.TracksFor(p.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		rows = append(rows, playlistRow{Playlist: p, Counts: countTracks(tracks)})
	}
	s.render(w, "index", map[string]any{
		"Playlists":  rows,
		"Total":      total,
		"Page":       page,
		"TotalPages": totalPages,
		"HasPrev":    page > 1,
		"HasNext":    page < totalPages,
		"PrevPage":   page - 1,
		"NextPage":   page + 1,
		"StartNum":   start, // offset for global numbering
	})
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	pl, err := s.store.PlaylistByID(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if pl == nil {
		http.NotFound(w, r)
		return
	}
	tracks, err := s.store.TracksFor(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	// Order: position is already ascending from the store.
	rows := make([]trackRow, len(tracks))
	for i, t := range tracks {
		rows[i] = trackRow{Track: t}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Position < rows[j].Position })

	s.render(w, "detail", map[string]any{
		"Playlist":        pl,
		"Counts":          countTracks(tracks),
		"Tracks":          rows,
		"CanRetag":        s.retag != nil,
		"CanRescanLyrics": s.rescanLyrics != nil,
		"CanDiscard":      s.discard != nil,
		"CanResort":       s.resort != nil,
	})
}

func (s *Server) handleRetryTrack(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	playlistID, err := s.store.RetryTrack(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if playlistID == 0 {
		http.NotFound(w, r)
		return
	}
	s.log.Info("retry track requested", "track_id", id, "playlist_id", playlistID)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(playlistID, 10), http.StatusSeeOther)
}

func (s *Server) handleRetryMissing(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := s.store.RetryMissing(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("retry missing requested", "playlist_id", id, "reset", n)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleResync(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	n, err := s.store.ResyncPlaylist(id)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("resync requested", "playlist_id", id, "demoted", n)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleRescanLyrics(w http.ResponseWriter, r *http.Request) {
	if s.rescanLyrics == nil {
		http.Error(w, "lyrics fetching is disabled", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// A playlist can hold dozens of tracks, each needing a network lookup, so run
	// it in the background and return immediately; the dashboard reflects the new
	// statuses on the next reload. A detached context (not the request's) keeps it
	// alive after the redirect.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		n, err := s.rescanLyrics(ctx, id)
		if err != nil {
			s.log.Warn("rescan lyrics", "playlist_id", id, "err", err)
			return
		}
		s.log.Info("rescan lyrics done", "playlist_id", id, "processed", n)
	}()
	s.log.Info("rescan lyrics requested", "playlist_id", id)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleResort(w http.ResponseWriter, r *http.Request) {
	if s.resort == nil {
		http.Error(w, "resort is disabled", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.resort(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("resort requested", "playlist_id", id)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDiscard(w http.ResponseWriter, r *http.Request) {
	if s.discard == nil {
		http.Error(w, "discard is disabled", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	playlistID, err := s.discard(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if playlistID == 0 {
		http.NotFound(w, r)
		return
	}
	s.log.Info("discard download requested", "track_id", id, "playlist_id", playlistID)
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(playlistID, 10), http.StatusSeeOther)
}

func (s *Server) handleRefingerprint(w http.ResponseWriter, r *http.Request) {
	if s.retag == nil {
		http.Error(w, "fingerprinting is disabled", http.StatusNotFound)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	playlistID, terr := s.retag(r.Context(), id)
	if terr != nil {
		// Non-fatal: the daemon keeps retrying via rescan. Surface it and return
		// to the page rather than erroring out.
		s.log.Warn("tag mbid", "track_id", id, "err", terr)
	} else {
		s.log.Info("tag mbid requested", "track_id", id)
	}
	if playlistID == 0 {
		http.NotFound(w, r)
		return
	}
	http.Redirect(w, r, "/playlist/"+strconv.FormatInt(playlistID, 10), http.StatusSeeOther)
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("render template", "name", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("web handler", "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

// --- templates ---

var funcs = template.FuncMap{
	"dict": func(values ...any) map[string]any {
		m := make(map[string]any, len(values)/2)
		for i := 0; i+1 < len(values); i += 2 {
			key, _ := values[i].(string)
			m[key] = values[i+1]
		}
		return m
	},
	"isMissing": func(st store.TrackStatus) bool { return st == store.TrackMissing },
	"isDone":    func(st store.PlaylistStatus) bool { return st == store.PlaylistDone },
	// imported-but-unmatched tracks (stuck awaiting a Navidrome match) are the
	// ones a manual re-fingerprint can help.
	"isStuck": func(st store.TrackStatus) bool {
		return st == store.TrackDownloaded || st == store.TrackImported
	},
	"statusClass": func(st store.TrackStatus) string {
		switch st {
		case store.TrackInPlaylist:
			return "s-inplaylist"
		case store.TrackDownloading:
			return "s-downloading"
		case store.TrackDownloaded:
			return "s-downloaded"
		case store.TrackExists:
			return "s-exists"
		case store.TrackMissing:
			return "s-missing"
		default:
			return "s-pending"
		}
	},
	"shortTime": func(t time.Time) string {
		if t.IsZero() {
			return "—"
		}
		return t.Format("Jan 2 15:04")
	},
	"base": func(p string) string {
		for i := len(p) - 1; i >= 0; i-- {
			if p[i] == '/' {
				return p[i+1:]
			}
		}
		return p
	},
	"pad2": func(n int) string {
		s := strconv.Itoa(n)
		if len(s) < 2 {
			return "0" + s
		}
		return s
	},
	"inc": func(n int) int { return n + 1 },
	"add": func(a, b int) int { return a + b },
}

var tmpl = template.Must(template.New("").Funcs(funcs).Parse(pageTemplates))

const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} · Weekly Jams</title>
<style>
  :root, :root[data-theme="dark"] {
    color-scheme: dark;
    --bg: #0b0c10;
    --panel: rgba(255,255,255,.028);
    --panel-2: rgba(255,255,255,.05);
    --line: rgba(255,255,255,.09);
    --line-2: rgba(255,255,255,.16);
    --ink: #ece9e2;
    --ink-2: #c4c0b5;
    --muted: #8c897d;
    --amber: #f4a94a;
    --green: #46cd7d;
    --blue: #62a6f2;
    --teal: #43c9c9;
    --red: #ef6a6a;
    --track: rgba(255,255,255,.07);
    --hover: rgba(255,255,255,.09);
    --glow-amber: rgba(244,169,74,.12);
    --glow-blue: rgba(98,166,242,.09);
  }
  :root[data-theme="light"] {
    color-scheme: light;
    --bg: #f4efe4;
    --panel: rgba(0,0,0,.022);
    --panel-2: rgba(0,0,0,.045);
    --line: rgba(0,0,0,.1);
    --line-2: rgba(0,0,0,.18);
    --ink: #241f18;
    --ink-2: #4b4536;
    --muted: #6b6252;
    --amber: #a9660f;
    --green: #157a41;
    --blue: #2a5fb8;
    --teal: #167d7d;
    --red: #b8362f;
    --track: rgba(0,0,0,.09);
    --hover: rgba(0,0,0,.06);
    --glow-amber: rgba(212,150,60,.22);
    --glow-blue: rgba(70,120,200,.12);
  }
  :root {
    --serif: "Iowan Old Style", "Palatino Linotype", Palatino, "Book Antiqua", Georgia, serif;
    --mono: ui-monospace, "SF Mono", "JetBrains Mono", "Cascadia Code", Menlo, Consolas, monospace;
    --radius: 14px;
  }
  * { box-sizing: border-box; }
  html { -webkit-text-size-adjust: 100%; }
  body {
    font-family: var(--mono);
    color: var(--ink);
    margin: 0;
    padding: 2.2rem 1.5rem 3rem;
    max-width: 1040px;
    margin-inline: auto;
    line-height: 1.5;
    background:
      radial-gradient(1100px 620px at 8% -10%, var(--glow-amber), transparent 58%),
      radial-gradient(900px 560px at 112% 4%, var(--glow-blue), transparent 55%),
      var(--bg);
    background-attachment: fixed;
  }
  body::before {
    content: ""; position: fixed; inset: 0; pointer-events: none; z-index: -1;
    background: repeating-linear-gradient(0deg, rgba(255,255,255,.014) 0 1px, transparent 1px 3px);
    mix-blend-mode: overlay;
  }
  a { color: inherit; text-decoration: none; }
  h1 { font-family: var(--serif); font-weight: 600; font-size: 2rem; letter-spacing: -.01em; margin: 0; }
  h2 { font-family: var(--serif); font-weight: 600; font-size: 1.15rem; margin: 0; letter-spacing: -.005em; }
  .muted { color: var(--muted); font-size: .82rem; }
  .kicker { font-size: .68rem; letter-spacing: .22em; text-transform: uppercase; color: var(--amber); }

  /* --- masthead --- */
  .masthead { display: flex; align-items: center; gap: 1.1rem; padding-bottom: 1.4rem; margin-bottom: 1.8rem; border-bottom: 1px solid var(--line); }
  .disc {
    flex: none; width: 46px; height: 46px; border-radius: 50%;
    background:
      radial-gradient(circle at 50% 50%, #d6d2c8 0 13%, transparent 14%),
      radial-gradient(circle at 50% 50%, var(--amber) 0 20%, transparent 21%),
      repeating-radial-gradient(circle at 50% 50%, #16171c 0 1.5px, #101116 1.5px 3px),
      #0a0a0d;
    box-shadow: 0 0 0 1px var(--line-2), 0 6px 20px rgba(0,0,0,.5);
    animation: spin 8s linear infinite;
  }
  .masthead .title-block { flex: 1; }
  .masthead .count-badge { text-align: right; }
  .masthead .count-badge b { font-family: var(--serif); font-size: 1.7rem; color: var(--ink); display: block; line-height: 1; }
  @keyframes spin { to { transform: rotate(360deg); } }
  @media (prefers-reduced-motion: reduce) { .disc { animation: none; } }

  /* --- theme toggle --- */
  .theme-toggle { flex: none; width: 32px; height: 32px; display: inline-flex; align-items: center; justify-content: center; padding: 0; border: 0; background: none; color: var(--muted); cursor: pointer; border-radius: 50%; opacity: .8; transition: color .15s, opacity .15s, background .15s; }
  .theme-toggle:hover { color: var(--amber); opacity: 1; background: var(--hover); }
  .theme-toggle svg { width: 17px; height: 17px; display: block; }
  .theme-toggle .ico-moon { display: none; }
  :root[data-theme="light"] .theme-toggle .ico-sun { display: none; }
  :root[data-theme="light"] .theme-toggle .ico-moon { display: block; }

  /* --- meter --- */
  .meter { height: 6px; border-radius: 6px; background: var(--track); overflow: hidden; box-shadow: inset 0 0 0 1px rgba(0,0,0,.2); }
  .meter > span { display: block; height: 100%; border-radius: 6px; background: linear-gradient(90deg, var(--green), #7fe0a3); box-shadow: 0 0 12px rgba(70,205,125,.5); transition: width .5s ease; }

  /* --- index cards --- */
  .stack { display: flex; flex-direction: column; gap: .7rem; }
  .card { display: grid; grid-template-columns: auto 1fr; gap: 1rem; align-items: center; padding: 1.05rem 1.15rem; border: 1px solid var(--line); border-radius: var(--radius); background: var(--panel); box-shadow: inset 3px 0 0 var(--line-2); transition: border-color .18s, background .18s, transform .18s, box-shadow .18s; }
  .card:hover { border-color: var(--line-2); background: var(--panel-2); transform: translateY(-2px); }
  .card.is-done { box-shadow: inset 3px 0 0 var(--green); }
  .card.is-done .num { color: var(--green); }
  .card .num { font-size: .78rem; color: var(--muted); width: 1.5rem; text-align: right; padding-top: .1rem; }
  .card .body { min-width: 0; }
  .card .top { display: flex; justify-content: space-between; align-items: baseline; gap: 1rem; margin-bottom: .55rem; }
  .card h2 { white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .card .pct { font-size: 1.05rem; color: var(--amber); flex: none; }
  .card .foot { display: flex; justify-content: space-between; align-items: center; gap: 1rem; margin-top: .55rem; }

  /* --- pills --- */
  .pills { display: flex; flex-wrap: wrap; gap: .35rem; align-items: center; }
  .pills span { font-size: .68rem; padding: .12rem .5rem; border-radius: 999px; border: 1px solid var(--line-2); color: var(--muted); white-space: nowrap; display: inline-flex; align-items: center; gap: .28rem; }
  .ico { width: .95em; height: .95em; flex: none; }
  .status-tag { font-size: .68rem; letter-spacing: .12em; text-transform: uppercase; color: var(--muted); display: inline-flex; align-items: center; gap: .35rem; white-space: nowrap; }
  .status-tag.is-done { color: var(--green); }

  /* --- detail toolbar --- */
  .head-row { display: flex; justify-content: space-between; align-items: center; margin-bottom: .9rem; }
  .backlink { display: inline-flex; align-items: center; gap: .35rem; font-size: .74rem; letter-spacing: .06em; color: var(--muted); }
  .backlink:hover { color: var(--amber); }
  .detail-head { border-bottom: 1px solid var(--line); padding-bottom: 1.3rem; margin-bottom: 1.6rem; }
  .detail-head .top { display: flex; justify-content: space-between; align-items: flex-end; gap: 1.5rem; flex-wrap: wrap; margin-bottom: 1rem; }
  .placed { text-align: right; font-size: .8rem; color: var(--muted); }
  .placed b { font-family: var(--serif); font-size: 1.6rem; color: var(--ink); }
  .toolbar { display: flex; flex-wrap: wrap; gap: .45rem; margin-top: 1rem; }

  /* --- buttons --- */
  form.inline { display: inline; margin: 0; }
  button { font-family: var(--mono); font-size: .74rem; cursor: pointer; border: 1px solid var(--line-2); background: var(--panel-2); color: var(--ink); border-radius: 8px; padding: .3rem .7rem; transition: border-color .15s, background .15s, color .15s; }
  button:hover { border-color: var(--muted); background: var(--hover); }
  button.primary { border-color: rgba(244,169,74,.55); color: var(--amber); background: rgba(244,169,74,.1); }
  button.primary:hover { background: rgba(244,169,74,.2); }

  /* --- track cards --- */
  .tracks { display: flex; flex-direction: column; gap: .5rem; }
  .track { display: grid; grid-template-columns: auto 1fr; gap: 1rem; padding: .85rem 1.05rem; border: 1px solid var(--line); border-radius: 12px; background: var(--panel); transition: border-color .15s, background .15s; }
  .track:hover { border-color: var(--line-2); background: var(--panel-2); }
  .track.is-missing { border-color: rgba(239,106,106,.28); background: rgba(239,106,106,.035); }
  .track .num { font-size: .8rem; color: var(--muted); width: 1.7rem; text-align: right; padding-top: .2rem; font-variant-numeric: tabular-nums; }
  .track .body { min-width: 0; }
  .track .body > * + * { margin-top: .5rem; }
  .track .headline { display: flex; justify-content: space-between; align-items: baseline; gap: 1rem; }
  .track .tname { font-family: var(--serif); font-size: 1.04rem; min-width: 0; }
  .track .tname .ta { color: var(--ink-2); }
  .track .tname .sep { color: var(--muted); margin: 0 .4rem; }
  .track .tname .tt { color: var(--ink); }
  .track-src { font-size: .68rem; color: var(--muted); word-break: break-all; font-family: var(--mono); }
  .track-actions { display: flex; flex-wrap: wrap; gap: .4rem; }
  .track-meta { font-size: .72rem; color: var(--muted); display: flex; flex-wrap: wrap; align-items: center; }
  .track-meta .mi + .mi::before { content: "·"; color: var(--line-2); margin: 0 .5rem; }

  /* --- status chips --- */
  .st { font-size: .66rem; letter-spacing: .04em; padding: .16rem .55rem; border-radius: 999px; white-space: nowrap; display: inline-flex; align-items: center; gap: .3rem; border: 1px solid transparent; }
  .st::before { content: ""; width: 6px; height: 6px; border-radius: 50%; background: currentColor; }
  .s-inplaylist { color: var(--green); background: rgba(70,205,125,.12); border-color: rgba(70,205,125,.3); }
  .s-exists { color: var(--green); background: rgba(143,208,106,.14); border-color: rgba(143,208,106,.35); }
  .s-downloading { color: var(--blue); background: rgba(98,166,242,.12); border-color: rgba(98,166,242,.3); }
  .s-downloaded { color: var(--teal); background: rgba(67,201,201,.12); border-color: rgba(67,201,201,.3); }
  .s-missing { color: var(--red); background: rgba(239,106,106,.12); border-color: rgba(239,106,106,.3); }
  .s-pending { color: var(--muted); background: rgba(255,255,255,.05); border-color: var(--line-2); }
  .s-lyrics-synced { color: var(--green); background: rgba(70,205,125,.1); border-color: rgba(70,205,125,.25); }
  .s-lyrics-plain { color: var(--amber); background: rgba(244,169,74,.1); border-color: rgba(244,169,74,.25); }
  .st.s-lyrics-synced::before, .st.s-lyrics-plain::before { display: none; }

  .err { color: var(--red); font-size: .72rem; max-width: 22rem; }
  .src { font-size: .68rem; color: var(--muted); word-break: break-all; }
  .empty { text-align: center; padding: 4rem 1rem; color: var(--muted); border: 1px dashed var(--line-2); border-radius: var(--radius); }

  /* --- pagination --- */
  .pager { display: flex; align-items: center; justify-content: center; gap: 1rem; margin-top: 1.6rem; }
  .pager a, .pager .disabled { display: inline-flex; align-items: center; gap: .35rem; font-size: .74rem; letter-spacing: .06em; padding: .4rem .85rem; border: 1px solid var(--line-2); border-radius: 8px; background: var(--panel-2); color: var(--ink); transition: border-color .15s, background .15s, color .15s; }
  .pager a:hover { border-color: var(--muted); background: var(--hover); color: var(--amber); }
  .pager .disabled { opacity: .4; pointer-events: none; }
  .pager .page-info { font-size: .72rem; letter-spacing: .1em; text-transform: uppercase; color: var(--muted); }
</style>
<script>
(function(){try{var t=localStorage.getItem('theme');if(t!=='light'&&t!=='dark'){t=window.matchMedia&&window.matchMedia('(prefers-color-scheme: light)').matches?'light':'dark';}document.documentElement.setAttribute('data-theme',t);}catch(e){}})();
</script>
</head><body>{{end}}

{{define "foot"}}</body></html>{{end}}

{{define "ico-pending"}}<svg class="ico" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="8" cy="8" r="6.25"/><path d="M8 4.5V8l2.4 1.6"/></svg>{{end}}

{{define "ico-done"}}<svg class="ico" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.9" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M3 8.4l3.2 3.1L13 4.5"/></svg>{{end}}

{{define "theme-toggle"}}<button class="theme-toggle" type="button" title="Toggle light / dark theme" aria-label="Toggle light or dark theme" onclick="var r=document.documentElement,n=r.getAttribute('data-theme')==='light'?'dark':'light';r.setAttribute('data-theme',n);try{localStorage.setItem('theme',n)}catch(e){}"><svg class="ico-sun" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><circle cx="12" cy="12" r="4"/><path d="M12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"/></svg><svg class="ico-moon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20.5 14.8A8.2 8.2 0 019.2 3.5 7.3 7.3 0 1020.5 14.8z"/></svg></button>{{end}}

{{define "index"}}
{{template "head" (dict "Title" "Playlists")}}
<div class="masthead">
  <span class="disc" aria-hidden="true"></span>
  <div class="title-block">
    <div class="kicker">ListenBrainz → Navidrome</div>
    <h1>Weekly Jams</h1>
  </div>
  <div class="count-badge"><b>{{.Total}}</b><span class="muted">playlists</span></div>
  {{template "theme-toggle"}}
</div>
{{if not .Playlists}}
  <div class="empty">No playlists discovered yet — they appear here as feeds are read.</div>
{{else}}
<div class="stack">
{{range $i, $p := .Playlists}}
<a class="card{{if isDone $p.Status}} is-done{{end}}" href="/playlist/{{$p.ID}}">
  <span class="num">{{pad2 (inc (add $.StartNum $i))}}</span>
  <div class="body">
    <div class="top">
      <h2>{{$p.Title}}</h2>
      <span class="pct">{{$p.Counts.Percent}}%</span>
    </div>
    <div class="meter"><span style="width:{{$p.Counts.Percent}}%"></span></div>
    <div class="foot">
      <div class="pills">
        <span>♫ {{$p.Counts.InPlaylist}}/{{$p.Counts.Total}}</span>
        <span>◍ {{$p.NavidromeUser}}</span>
        {{if $p.Counts.Downloading}}<span>⬇ {{$p.Counts.Downloading}}</span>{{end}}
        {{if $p.Counts.Missing}}<span>✗ {{$p.Counts.Missing}}</span>{{end}}
        {{if $p.Counts.Pending}}<span>{{template "ico-pending"}} {{$p.Counts.Pending}}</span>{{end}}
      </div>
      {{if isDone $p.Status}}<span class="status-tag is-done">{{template "ico-done"}} done</span>
      {{else}}<span class="status-tag">{{template "ico-pending"}} {{$p.Status}}</span>{{end}}
    </div>
  </div>
</a>
{{end}}
</div>
{{if gt .TotalPages 1}}
<nav class="pager">
  {{if .HasPrev}}<a href="/?page={{.PrevPage}}">← newer</a>{{else}}<span class="disabled">← newer</span>{{end}}
  <span class="page-info">page {{.Page}} of {{.TotalPages}}</span>
  {{if .HasNext}}<a href="/?page={{.NextPage}}">older →</a>{{else}}<span class="disabled">older →</span>{{end}}
</nav>
{{end}}
{{end}}
{{template "foot"}}
{{end}}

{{define "detail"}}
{{template "head" (dict "Title" .Playlist.Title)}}
<div class="detail-head">
  <div class="head-row">
    <a class="backlink" href="/">← all playlists</a>
    {{template "theme-toggle"}}
  </div>
  <div class="top">
    <div>
      <div class="kicker">{{.Playlist.NavidromeUser}} · {{.Playlist.Status}}</div>
      <h1>{{.Playlist.Title}}</h1>
    </div>
    <div class="placed"><b>{{.Counts.InPlaylist}}</b> / {{.Counts.Total}}<br>placed · {{.Counts.Percent}}%</div>
  </div>
  <div class="meter"><span style="width:{{.Counts.Percent}}%"></span></div>
  <div class="pills" style="margin-top:1rem">
    {{if .Counts.Downloading}}<span>⬇ {{.Counts.Downloading}} downloading</span>{{end}}
    {{if .Counts.Downloaded}}<span>↯ {{.Counts.Downloaded}} importing</span>{{end}}
    {{if .Counts.Exists}}<span>✓ {{.Counts.Exists}} in library</span>{{end}}
    {{if .Counts.Missing}}<span>✗ {{.Counts.Missing}} missing</span>{{end}}
    {{if .Counts.Pending}}<span>{{template "ico-pending"}} {{.Counts.Pending}} pending</span>{{end}}
  </div>
  <div class="toolbar">
    <form class="inline" method="post" action="/playlist/{{.Playlist.ID}}/resync">
      <button type="submit" title="Re-check the Navidrome playlist and re-add any songs it is missing">⟳ Re-sync</button>
    </form>
    {{if and .CanResort (isDone .Playlist.Status)}}
    <form class="inline" method="post" action="/playlist/{{.Playlist.ID}}/resort">
      <button type="submit" title="Reorder the Navidrome playlist to match the original feed order">↕ Sort to feed order</button>
    </form>
    {{end}}
    {{if .CanRescanLyrics}}
    <form class="inline" method="post" action="/playlist/{{.Playlist.ID}}/rescan-lyrics">
      <button type="submit" title="Fetch missing lyrics (.lrc) for every imported track in this playlist (runs in the background)">♪ Re-scan lyrics</button>
    </form>
    {{end}}
    {{if .Counts.Missing}}
    <form class="inline" method="post" action="/playlist/{{.Playlist.ID}}/retry-missing">
      <button class="primary" type="submit">↻ Retry {{.Counts.Missing}} missing</button>
    </form>
    {{end}}
  </div>
</div>
<div class="tracks">
{{range .Tracks}}
  <div class="track{{if isMissing .Status}} is-missing{{end}}">
    <span class="num">{{pad2 .Position}}</span>
    <div class="body">
      <div class="headline">
        <div class="tname"><span class="ta">{{.Artist}}</span><span class="sep">—</span><span class="tt">{{.Title}}</span></div>
        <span class="st {{statusClass .Status}}">{{.Status}}</span>
      </div>
      <div class="track-meta">
        {{if eq .LyricsStatus "synced"}}<span class="mi st s-lyrics-synced">♪ synced</span>
        {{else if eq .LyricsStatus "plain"}}<span class="mi st s-lyrics-plain">♪ plain</span>{{end}}
        {{if eq .Source "ytdlp"}}<span class="mi" title="acquired via the yt-dlp fallback (top YouTube hit) — verify quality">▶ yt-dlp</span>{{end}}
        {{if .SlskdUsername}}<span class="mi">⬇ {{.SlskdUsername}}</span>{{end}}
        {{if .Attempts}}<span class="mi">{{.Attempts}} {{if eq .Attempts 1}}try{{else}}tries{{end}}</span>{{end}}
        {{if .ImportedPath}}<span class="mi">{{base .ImportedPath}}</span>{{end}}
        <span class="mi">updated {{shortTime .UpdatedAt}}</span>
      </div>
      {{if .SlskdFile}}<div class="track-src" title="original download (peer path)">⇩ {{.SlskdFile}}</div>{{end}}
      {{if .LastError}}<div class="err">⚠ {{.LastError}}</div>{{end}}
      {{if or (isMissing .Status) (and $.CanRetag (isStuck .Status)) (and $.CanDiscard (isStuck .Status))}}
      <div class="track-actions">
        {{if isMissing .Status}}
        <form class="inline" method="post" action="/track/{{.ID}}/retry">
          <button type="submit">↻ Retry</button>
        </form>
        {{end}}
        {{if and $.CanRetag (isStuck .Status)}}
        <form class="inline" method="post" action="/track/{{.ID}}/refingerprint">
          <button type="submit" title="Write the feed's recording MBID into the file's tags + rescan so Navidrome can match this imported file">⊕ Tag MBID</button>
        </form>
        {{end}}
        {{if and $.CanDiscard (isStuck .Status)}}
        <form class="inline" method="post" action="/track/{{.ID}}/discard" onsubmit="return confirm('Delete the downloaded file and search for this track again?')">
          <button type="submit" title="Delete the wrong downloaded file and search for this track again">🗑 Delete &amp; restart</button>
        </form>
        {{end}}
      </div>
      {{end}}
    </div>
  </div>
{{end}}
</div>
{{template "foot"}}
{{end}}
`
