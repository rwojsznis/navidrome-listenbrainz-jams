// Package fingerprint implements the optional acoustic-identification step: it
// fingerprints a downloaded file with Chromaprint (fpcalc), resolves the
// fingerprint to a MusicBrainz recording id via AcoustID, and writes that id
// into the file's tags so Navidrome indexes it. It satisfies the downloader's
// tagger hook.
//
// The step trusts the download (it does not reject mismatches): when AcoustID
// returns the feed's own recording id among the candidates we prefer it,
// otherwise we take the best-scoring result. When AcoustID returns nothing the
// file is left untagged — never a hard failure.
package fingerprint

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/config"
	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/tags"
)

// lookupURL is the AcoustID lookup endpoint (a var so tests can point it at a
// stub server).
var lookupURL = "https://api.acoustid.org/v2/lookup"

// Service identifies and tags downloaded files.
type Service struct {
	apiKey     string
	fpcalcPath string
	httpClient *http.Client
	tagger     tags.Writer
	log        *slog.Logger
}

// New builds a Service from config. The caller wires it into the downloader only
// when cfg.Fingerprint.Enabled.
func New(cfg *config.Config, log *slog.Logger) *Service {
	fpcalc := cfg.Fingerprint.FpcalcPath
	if fpcalc == "" {
		fpcalc = "fpcalc"
	}
	return &Service{
		apiKey:     cfg.Fingerprint.AcoustIDAPIKey,
		fpcalcPath: fpcalc,
		httpClient: &http.Client{Timeout: 20 * time.Second},
		tagger:     tags.Writer{OpustagsPath: cfg.Fingerprint.OpustagsPath},
		log:        log,
	}
}

// Tag fingerprints path, resolves a recording MBID via AcoustID, and writes it
// into the file. preferMBID is the feed's recording id, chosen when AcoustID
// lists it among the candidates. A missing AcoustID match is not an error.
func (s *Service) Tag(ctx context.Context, path, preferMBID string) error {
	fp, duration, err := s.fingerprint(ctx, path)
	if err != nil {
		return fmt.Errorf("fingerprint: %w", err)
	}
	mbids, err := s.lookup(ctx, fp, duration)
	if err != nil {
		return fmt.Errorf("acoustid lookup: %w", err)
	}
	mbid, ok := choose(mbids, preferMBID)
	if !ok {
		s.log.Info("no acoustid match; leaving untagged", "path", path)
		return nil
	}
	if err := s.tagger.WriteRecordingMBID(ctx, path, mbid); err != nil {
		return fmt.Errorf("write tag: %w", err)
	}
	s.log.Info("tagged recording mbid", "path", path, "mbid", mbid, "from_feed", mbid == preferMBID)
	return nil
}

type fpcalcResult struct {
	Duration    float64 `json:"duration"`
	Fingerprint string  `json:"fingerprint"`
}

// fingerprint runs `fpcalc -json` to get the compressed (AcoustID-compatible)
// fingerprint and the audio duration.
func (s *Service) fingerprint(ctx context.Context, path string) (string, float64, error) {
	out, err := exec.CommandContext(ctx, s.fpcalcPath, "-json", path).Output()
	if err != nil {
		return "", 0, fmt.Errorf("run fpcalc: %w", err)
	}
	var r fpcalcResult
	if err := json.Unmarshal(out, &r); err != nil {
		return "", 0, fmt.Errorf("parse fpcalc output: %w", err)
	}
	if r.Fingerprint == "" || r.Duration <= 0 {
		return "", 0, fmt.Errorf("empty fingerprint")
	}
	return r.Fingerprint, r.Duration, nil
}

// acoustIDResponse is the subset of the lookup response we consume. Results are
// ordered by descending score.
type acoustIDResponse struct {
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
	} `json:"error"`
	Results []struct {
		Score      float64 `json:"score"`
		Recordings []struct {
			ID string `json:"id"`
		} `json:"recordings"`
	} `json:"results"`
}

// lookup queries AcoustID and returns candidate recording MBIDs, best score
// first. The fingerprint is long, so we POST a form body rather than a query
// string.
func (s *Service) lookup(ctx context.Context, fingerprint string, duration float64) ([]string, error) {
	form := url.Values{}
	form.Set("client", s.apiKey) // AcoustID *application* key (not the user key)
	form.Set("duration", strconv.Itoa(int(duration+0.5)))
	form.Set("fingerprint", fingerprint)
	form.Set("meta", "recordingids")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, lookupURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// AcoustID returns a JSON error body even on non-2xx (e.g. 400 with
	// "invalid API key"), so decode first and prefer its message over the bare
	// status code — otherwise misconfiguration looks like an opaque "http 400".
	var ar acoustIDResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return nil, fmt.Errorf("http %d: unparseable response: %w", resp.StatusCode, err)
	}
	if ar.Status != "ok" {
		if ar.Error.Message != "" {
			return nil, fmt.Errorf("%s (http %d)", ar.Error.Message, resp.StatusCode)
		}
		return nil, fmt.Errorf("http %d", resp.StatusCode)
	}

	var mbids []string
	for _, r := range ar.Results {
		for _, rec := range r.Recordings {
			if rec.ID != "" {
				mbids = append(mbids, rec.ID)
			}
		}
	}
	return mbids, nil
}

// choose prefers the feed's recording id when AcoustID lists it (a confirmed
// match), otherwise returns the best-scoring candidate. ok is false when there
// are no candidates.
func choose(mbids []string, prefer string) (string, bool) {
	if len(mbids) == 0 {
		return "", false
	}
	if prefer != "" {
		for _, m := range mbids {
			if m == prefer {
				return m, true
			}
		}
	}
	return mbids[0], true
}
