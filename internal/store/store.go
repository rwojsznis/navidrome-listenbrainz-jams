// Package store persists pipeline state in SQLite so playlist assembly survives
// restarts and resumes instead of re-downloading. It uses the pure-Go
// modernc.org/sqlite driver (no cgo) for clean cross-arch Docker builds.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// PlaylistStatus tracks overall progress of a playlist's assembly.
type PlaylistStatus string

const (
	PlaylistPending PlaylistStatus = "pending" // discovered, not yet fully assembled
	PlaylistDone    PlaylistStatus = "done"    // every track is in_playlist or missing
)

// TrackStatus is the per-track position in the assembly state machine.
type TrackStatus string

const (
	TrackPending     TrackStatus = "pending"     // not yet processed this run
	TrackExists      TrackStatus = "exists"      // present in Navidrome, songID resolved
	TrackDownloading TrackStatus = "downloading" // slskd transfer in flight
	TrackDownloaded  TrackStatus = "downloaded"  // file completed, awaiting import
	TrackImported    TrackStatus = "imported"    // moved into library, awaiting rescan match
	TrackInPlaylist  TrackStatus = "in_playlist" // added to the Navidrome playlist
	TrackMissing     TrackStatus = "missing"     // not found / exhausted retries (retried later)
)

// Playlist is a stored playlist row.
type Playlist struct {
	ID                  int64
	FeedName            string
	LBEntryID           string
	Title               string
	NavidromeUser       string
	NavidromePlaylistID string
	Status              PlaylistStatus
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Track is a stored track row.
type Track struct {
	ID              int64
	PlaylistID      int64
	Position        int
	RecordingMBID   string
	Artist          string
	Title           string
	Status          TrackStatus
	NavidromeSongID string
	SlskdUsername   string
	SlskdFile       string
	ImportedPath    string
	Attempts        int
	LastError       string
	// LyricsStatus records the outcome of the optional lyrics step: "synced",
	// "plain", "none" (looked up, nothing found), or "" (not yet attempted).
	LyricsStatus string
	// Source records which backend acquired the track: "" (not yet acquired),
	// "slskd", or "ytdlp". It provides dashboard provenance and makes the yt-dlp
	// fallback a one-shot (see internal/downloadchain).
	Source    string
	UpdatedAt time.Time
}

// Store wraps the SQLite connection.
type Store struct {
	db *sql.DB
}

const schema = `
CREATE TABLE IF NOT EXISTS playlists (
	id                    INTEGER PRIMARY KEY AUTOINCREMENT,
	feed_name             TEXT NOT NULL,
	lb_entry_id           TEXT NOT NULL UNIQUE,
	title                 TEXT NOT NULL,
	navidrome_user        TEXT NOT NULL,
	navidrome_playlist_id TEXT NOT NULL DEFAULT '',
	status                TEXT NOT NULL DEFAULT 'pending',
	created_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	updated_at            TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS tracks (
	id                INTEGER PRIMARY KEY AUTOINCREMENT,
	playlist_id       INTEGER NOT NULL REFERENCES playlists(id) ON DELETE CASCADE,
	position          INTEGER NOT NULL,
	recording_mbid    TEXT NOT NULL,
	artist            TEXT NOT NULL,
	title             TEXT NOT NULL,
	status            TEXT NOT NULL DEFAULT 'pending',
	navidrome_song_id TEXT NOT NULL DEFAULT '',
	slskd_username    TEXT NOT NULL DEFAULT '',
	slskd_file        TEXT NOT NULL DEFAULT '',
	imported_path     TEXT NOT NULL DEFAULT '',
	attempts          INTEGER NOT NULL DEFAULT 0,
	last_error        TEXT NOT NULL DEFAULT '',
	lyrics_status     TEXT NOT NULL DEFAULT '',
	source            TEXT NOT NULL DEFAULT '',
	updated_at        TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
	UNIQUE(playlist_id, recording_mbid)
);

CREATE INDEX IF NOT EXISTS idx_tracks_playlist ON tracks(playlist_id);
`

// Open opens (creating if needed) the SQLite database at path and applies the
// schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite handles a single writer; keep one connection to avoid lock churn.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}
	return &Store{db: db}, nil
}

// migrate applies column additions to databases created before those columns
// existed. CREATE TABLE IF NOT EXISTS never alters an existing table, so each
// added column needs an explicit, idempotent ALTER (a "duplicate column" error
// means it is already present).
func migrate(db *sql.DB) error {
	adds := []string{
		`ALTER TABLE tracks ADD COLUMN lyrics_status TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE tracks ADD COLUMN source TEXT NOT NULL DEFAULT ''`,
	}
	for _, stmt := range adds {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return err
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// UpsertPlaylist inserts a playlist keyed by its ListenBrainz entry id, or
// returns the existing row if already present. It never overwrites an existing
// row's mutable state.
func (s *Store) UpsertPlaylist(feedName, lbEntryID, title, navidromeUser string) (*Playlist, error) {
	_, err := s.db.Exec(`
		INSERT INTO playlists (feed_name, lb_entry_id, title, navidrome_user)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(lb_entry_id) DO NOTHING`,
		feedName, lbEntryID, title, navidromeUser)
	if err != nil {
		return nil, fmt.Errorf("upsert playlist: %w", err)
	}
	return s.PlaylistByEntryID(lbEntryID)
}

// PlaylistByEntryID returns the playlist with the given ListenBrainz entry id.
func (s *Store) PlaylistByEntryID(lbEntryID string) (*Playlist, error) {
	row := s.db.QueryRow(`
		SELECT id, feed_name, lb_entry_id, title, navidrome_user,
		       navidrome_playlist_id, status, created_at, updated_at
		FROM playlists WHERE lb_entry_id = ?`, lbEntryID)
	return scanPlaylist(row)
}

// AllPlaylists returns every playlist, newest first (for the UI).
func (s *Store) AllPlaylists() ([]Playlist, error) {
	rows, err := s.db.Query(`
		SELECT id, feed_name, lb_entry_id, title, navidrome_user,
		       navidrome_playlist_id, status, created_at, updated_at
		FROM playlists ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Playlist
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// PlaylistByID returns a single playlist, or nil if not found.
func (s *Store) PlaylistByID(id int64) (*Playlist, error) {
	row := s.db.QueryRow(`
		SELECT id, feed_name, lb_entry_id, title, navidrome_user,
		       navidrome_playlist_id, status, created_at, updated_at
		FROM playlists WHERE id = ?`, id)
	return scanPlaylist(row)
}

// ActivePlaylists returns playlists not yet marked done, for the daemon to advance.
func (s *Store) ActivePlaylists() ([]Playlist, error) {
	rows, err := s.db.Query(`
		SELECT id, feed_name, lb_entry_id, title, navidrome_user,
		       navidrome_playlist_id, status, created_at, updated_at
		FROM playlists WHERE status != ? ORDER BY created_at`, PlaylistDone)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Playlist
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// SetPlaylistStatus updates a playlist's status.
func (s *Store) SetPlaylistStatus(id int64, status PlaylistStatus) error {
	_, err := s.db.Exec(`UPDATE playlists SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, status, id)
	return err
}

// SetPlaylistNavidromeID records the created Navidrome playlist id.
func (s *Store) SetPlaylistNavidromeID(id int64, navidromePlaylistID string) error {
	_, err := s.db.Exec(`UPDATE playlists SET navidrome_playlist_id = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, navidromePlaylistID, id)
	return err
}

// UpsertTrack inserts a track keyed by (playlist, recording MBID), leaving an
// existing row's mutable state untouched.
func (s *Store) UpsertTrack(playlistID int64, position int, recordingMBID, artist, title string) error {
	_, err := s.db.Exec(`
		INSERT INTO tracks (playlist_id, position, recording_mbid, artist, title)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(playlist_id, recording_mbid) DO NOTHING`,
		playlistID, position, recordingMBID, artist, title)
	return err
}

// TracksFor returns all tracks for a playlist ordered by position.
func (s *Store) TracksFor(playlistID int64) ([]Track, error) {
	rows, err := s.db.Query(`
		SELECT id, playlist_id, position, recording_mbid, artist, title, status,
		       navidrome_song_id, slskd_username, slskd_file, imported_path,
		       attempts, last_error, lyrics_status, source, updated_at
		FROM tracks WHERE playlist_id = ? ORDER BY position`, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.PlaylistID, &t.Position, &t.RecordingMBID,
			&t.Artist, &t.Title, &t.Status, &t.NavidromeSongID, &t.SlskdUsername,
			&t.SlskdFile, &t.ImportedPath, &t.Attempts, &t.LastError, &t.LyricsStatus, &t.Source, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TrackByID returns a single track, or nil if not found.
func (s *Store) TrackByID(id int64) (*Track, error) {
	row := s.db.QueryRow(`
		SELECT id, playlist_id, position, recording_mbid, artist, title, status,
		       navidrome_song_id, slskd_username, slskd_file, imported_path,
		       attempts, last_error, lyrics_status, source, updated_at
		FROM tracks WHERE id = ?`, id)
	var t Track
	err := row.Scan(&t.ID, &t.PlaylistID, &t.Position, &t.RecordingMBID,
		&t.Artist, &t.Title, &t.Status, &t.NavidromeSongID, &t.SlskdUsername,
		&t.SlskdFile, &t.ImportedPath, &t.Attempts, &t.LastError, &t.LyricsStatus, &t.Source, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// RetryTrack resets a single track so the daemon re-attempts it, and reactivates
// its playlist. Returns the playlist id (for redirecting the UI), or 0 if the
// track does not exist.
func (s *Store) RetryTrack(trackID int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var playlistID int64
	err = tx.QueryRow(`SELECT playlist_id FROM tracks WHERE id = ?`, trackID).Scan(&playlistID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(resetTrackSQL+` WHERE id = ?`, TrackPending, trackID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE playlists SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, PlaylistPending, playlistID); err != nil {
		return 0, err
	}
	return playlistID, tx.Commit()
}

// RetryMissing resets every missing track in a playlist and reactivates the
// playlist. Returns how many tracks were reset.
func (s *Store) RetryMissing(playlistID int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(resetTrackSQL+` WHERE playlist_id = ? AND status = ?`, TrackPending, playlistID, TrackMissing)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if n > 0 {
		if _, err := tx.Exec(`UPDATE playlists SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, PlaylistPending, playlistID); err != nil {
			return 0, err
		}
	}
	return int(n), tx.Commit()
}

// ResyncPlaylist forces a reconcile against Navidrome without re-downloading:
// it demotes every in_playlist track (that has a resolved song id) back to
// "exists" and reactivates the playlist, so the next tick re-checks the real
// Navidrome contents and re-adds any songs the playlist is actually missing.
// Returns how many tracks were demoted.
func (s *Store) ResyncPlaylist(playlistID int64) (int, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`UPDATE tracks SET status = ?, updated_at = CURRENT_TIMESTAMP
		WHERE playlist_id = ? AND status = ? AND navidrome_song_id != ''`,
		TrackExists, playlistID, TrackInPlaylist)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := tx.Exec(`UPDATE playlists SET status = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`, PlaylistPending, playlistID); err != nil {
		return 0, err
	}
	return int(n), tx.Commit()
}

// resetTrackSQL clears a track's progress so it is searched/downloaded afresh.
// The caller appends a WHERE clause and supplies the new status as the first arg.
// Lyrics status is cleared too: a reset re-downloads the file, so any previously
// fetched lyrics state is stale. Source is cleared so a retried track starts over
// from slskd (rather than being treated as an exhausted yt-dlp one-shot).
const resetTrackSQL = `UPDATE tracks SET status = ?, attempts = 0, last_error = '',
	slskd_username = '', slskd_file = '', navidrome_song_id = '',
	imported_path = '', lyrics_status = '', source = '', updated_at = CURRENT_TIMESTAMP`

// UpdateTrack persists the mutable fields of a track.
func (s *Store) UpdateTrack(t *Track) error {
	_, err := s.db.Exec(`
		UPDATE tracks SET
			status = ?, navidrome_song_id = ?, slskd_username = ?, slskd_file = ?,
			imported_path = ?, attempts = ?, last_error = ?, lyrics_status = ?,
			source = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		t.Status, t.NavidromeSongID, t.SlskdUsername, t.SlskdFile,
		t.ImportedPath, t.Attempts, t.LastError, t.LyricsStatus, t.Source, t.ID)
	return err
}

// SetLyricsStatus updates only a track's lyrics_status column, leaving every
// other field (and updated_at) untouched. This is used by the manual lyrics
// re-scan, which can run concurrently with the daemon's own track updates;
// touching a single column avoids clobbering pipeline progress with a stale
// snapshot.
func (s *Store) SetLyricsStatus(trackID int64, status string) error {
	_, err := s.db.Exec(`UPDATE tracks SET lyrics_status = ? WHERE id = ?`, status, trackID)
	return err
}

type scanner interface {
	Scan(dest ...any) error
}

func scanPlaylist(sc scanner) (*Playlist, error) {
	var p Playlist
	err := sc.Scan(&p.ID, &p.FeedName, &p.LBEntryID, &p.Title, &p.NavidromeUser,
		&p.NavidromePlaylistID, &p.Status, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
