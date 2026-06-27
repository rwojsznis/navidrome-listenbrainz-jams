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
	rows := make([]playlistRow, 0, len(playlists))
	for _, p := range playlists {
		tracks, err := s.store.TracksFor(p.ID)
		if err != nil {
			s.fail(w, err)
			return
		}
		rows = append(rows, playlistRow{Playlist: p, Counts: countTracks(tracks)})
	}
	s.render(w, "index", map[string]any{"Playlists": rows})
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
}

var tmpl = template.Must(template.New("").Funcs(funcs).Parse(pageTemplates))

const pageTemplates = `
{{define "head"}}<!doctype html>
<html lang="en"><head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title>
<style>
  :root { color-scheme: light dark; }
  body { font-family: system-ui, -apple-system, Segoe UI, Roboto, sans-serif; margin: 0; padding: 1.5rem; max-width: 1000px; margin-inline: auto; }
  h1 { font-size: 1.3rem; } h2 { font-size: 1.05rem; font-weight: 600; }
  a { color: inherit; }
  .muted { color: #888; font-size: .85rem; }
  .card { border: 1px solid #8884; border-radius: 10px; padding: 1rem; margin-bottom: .8rem; text-decoration: none; display: block; }
  .card:hover { border-color: #888a; background: #8881; }
  .row { display: flex; justify-content: space-between; align-items: baseline; gap: 1rem; }
  .bar { height: 8px; border-radius: 4px; background: #8883; overflow: hidden; margin: .5rem 0; }
  .bar > span { display: block; height: 100%; background: #2e9e54; }
  .pills span { display: inline-block; font-size: .72rem; padding: .1rem .45rem; border-radius: 999px; margin-right: .3rem; border: 1px solid #8885; }
  table { width: 100%; border-collapse: collapse; font-size: .9rem; }
  th, td { text-align: left; padding: .4rem .5rem; border-bottom: 1px solid #8883; vertical-align: top; }
  th { font-size: .75rem; text-transform: uppercase; color: #888; }
  .st { font-size: .72rem; padding: .12rem .5rem; border-radius: 999px; white-space: nowrap; }
  .s-inplaylist { background:#2e9e5433; color:#2e9e54; } .s-exists { background:#5b9e2e33; color:#6fae3e; }
  .s-downloading { background:#2e74d033; color:#4a8fe0; } .s-downloaded { background:#2eb6b633; color:#2eb6b6; }
  .s-missing { background:#d04a4a33; color:#e06a6a; } .s-pending { background:#8883; color:#999; }
  .s-lyrics-synced { background:#2e9e5433; color:#2e9e54; } .s-lyrics-plain { background:#8a6d3b33; color:#b9923e; }
  .err { color:#e06a6a; font-size:.78rem; }
  .src { font-size:.72rem; color:#888; word-break:break-all; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; }
  .topbar { display:flex; justify-content:space-between; align-items:center; margin-bottom:1rem; }
  form.inline { display:inline; margin:0; }
  button { font: inherit; font-size:.78rem; cursor:pointer; border:1px solid #8886; background:#8881; color:inherit; border-radius:6px; padding:.15rem .55rem; }
  button:hover { background:#8883; }
  button.primary { border-color:#2e74d088; }
</style>
</head><body>{{end}}

{{define "foot"}}<p class="muted" style="margin-top:2rem">navidrome-listenbrainz-jams</p></body></html>{{end}}

{{define "index"}}
{{template "head" (dict "Title" "Playlists")}}
<div class="topbar"><h1>🎵 Weekly Jams</h1></div>
{{if not .Playlists}}<p class="muted">No playlists discovered yet.</p>{{end}}
{{range .Playlists}}
<a class="card" href="/playlist/{{.ID}}">
  <div class="row">
    <h2>{{.Title}}</h2>
    <span class="muted">{{.Counts.InPlaylist}}/{{.Counts.Total}} · {{.Counts.Percent}}%</span>
  </div>
  <div class="bar"><span style="width:{{.Counts.Percent}}%"></span></div>
  <div class="row">
    <div class="pills">
      <span>👤 {{.NavidromeUser}}</span>
      {{if .Counts.Downloading}}<span>⬇ {{.Counts.Downloading}} downloading</span>{{end}}
      {{if .Counts.Missing}}<span>✗ {{.Counts.Missing}} missing</span>{{end}}
      {{if .Counts.Pending}}<span>… {{.Counts.Pending}} pending</span>{{end}}
    </div>
    <span class="muted">{{.Status}}</span>
  </div>
</a>
{{end}}
{{template "foot"}}
{{end}}

{{define "detail"}}
{{template "head" (dict "Title" .Playlist.Title)}}
<div class="topbar">
  <div><a class="muted" href="/">← all playlists</a><h1>{{.Playlist.Title}}</h1></div>
  <div style="text-align:right">
    <div class="muted">{{.Counts.InPlaylist}}/{{.Counts.Total}} placed</div>
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
<div class="bar"><span style="width:{{.Counts.Percent}}%"></span></div>
<p class="pills">
  <span>👤 {{.Playlist.NavidromeUser}}</span>
  <span>{{.Playlist.Status}}</span>
  {{if .Counts.Downloading}}<span>⬇ {{.Counts.Downloading}}</span>{{end}}
  {{if .Counts.Missing}}<span>✗ {{.Counts.Missing}}</span>{{end}}
  {{if .Counts.Pending}}<span>… {{.Counts.Pending}}</span>{{end}}
</p>
<table>
  <thead><tr><th>#</th><th>Artist</th><th>Title</th><th>Status</th><th>Lyrics</th><th>Try</th><th>Updated</th><th>Detail</th></tr></thead>
  <tbody>
  {{range .Tracks}}
    <tr>
      <td class="muted">{{.Position}}</td>
      <td>{{.Artist}}</td>
      <td>{{.Title}}</td>
      <td><span class="st {{statusClass .Status}}">{{.Status}}</span></td>
      <td>
        {{if eq .LyricsStatus "synced"}}<span class="st s-lyrics-synced">♪ synced</span>
        {{else if eq .LyricsStatus "plain"}}<span class="st s-lyrics-plain">plain</span>
        {{else if eq .LyricsStatus "none"}}<span class="muted">none</span>
        {{else}}<span class="muted">—</span>{{end}}
      </td>
      <td class="muted">{{if .Attempts}}{{.Attempts}}{{else}}—{{end}}</td>
      <td class="muted">{{shortTime .UpdatedAt}}</td>
      <td>
        {{if .ImportedPath}}<span class="muted">{{base .ImportedPath}}</span>{{end}}
        {{if .SlskdUsername}}<span class="muted">⬇ {{.SlskdUsername}}</span>{{end}}
        {{if .SlskdFile}}<div class="src" title="original download (peer path)">⇩ {{.SlskdFile}}</div>{{end}}
        {{if .LastError}}<div class="err">{{.LastError}}</div>{{end}}
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
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
{{template "foot"}}
{{end}}
`
