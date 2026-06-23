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

// Server is the dashboard HTTP server.
type Server struct {
	store *store.Store
	log   *slog.Logger
	srv   *http.Server
}

// New builds a Server listening on addr (e.g. ":8080").
func New(st *store.Store, addr string, log *slog.Logger) *Server {
	s := &Server{store: st, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/playlist/{id}", s.handlePlaylist)
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
		"Playlist": pl,
		"Counts":   countTracks(tracks),
		"Tracks":   rows,
	})
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
  .err { color:#e06a6a; font-size:.78rem; }
  .topbar { display:flex; justify-content:space-between; align-items:center; margin-bottom:1rem; }
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
  <span class="muted">{{.Counts.InPlaylist}}/{{.Counts.Total}} placed</span>
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
  <thead><tr><th>#</th><th>Artist</th><th>Title</th><th>Status</th><th>Try</th><th>Updated</th><th>Detail</th></tr></thead>
  <tbody>
  {{range .Tracks}}
    <tr>
      <td class="muted">{{.Position}}</td>
      <td>{{.Artist}}</td>
      <td>{{.Title}}</td>
      <td><span class="st {{statusClass .Status}}">{{.Status}}</span></td>
      <td class="muted">{{if .Attempts}}{{.Attempts}}{{else}}—{{end}}</td>
      <td class="muted">{{shortTime .UpdatedAt}}</td>
      <td>
        {{if .ImportedPath}}<span class="muted">{{base .ImportedPath}}</span>{{end}}
        {{if .SlskdUsername}}<span class="muted">⬇ {{.SlskdUsername}}</span>{{end}}
        {{if .LastError}}<div class="err">{{.LastError}}</div>{{end}}
      </td>
    </tr>
  {{end}}
  </tbody>
</table>
{{template "foot"}}
{{end}}
`
