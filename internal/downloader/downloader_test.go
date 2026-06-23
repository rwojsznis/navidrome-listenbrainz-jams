package downloader

import (
	"reflect"
	"testing"

	"github.com/rwojsznis/navidrome-listenbrainz-jams/internal/store"
)

func TestSearchQueries(t *testing.T) {
	cases := []struct {
		name  string
		track store.Track
		want  []string
	}{
		{
			name:  "plain title: no simplified variant added",
			track: store.Track{Artist: "The Beatles", Title: "Come Together"},
			want:  []string{"The Beatles Come Together", "Come Together"},
		},
		{
			name:  "empty artist falls back to title only once",
			track: store.Track{Artist: "", Title: "Come Together"},
			want:  []string{"Come Together"},
		},
		{
			name:  "decorated title adds a simplified last-resort query",
			track: store.Track{Artist: "Queen", Title: "Bohemian Rhapsody (Remastered 2011)"},
			want: []string{
				"Queen Bohemian Rhapsody (Remastered 2011)",
				"Bohemian Rhapsody (Remastered 2011)",
				"Bohemian Rhapsody",
			},
		},
		{
			name:  "feat clause stripped in last resort",
			track: store.Track{Artist: "Eminem", Title: "Stan feat. Dido"},
			want: []string{
				"Eminem Stan feat. Dido",
				"Stan feat. Dido",
				"Stan",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := searchQueries(&tc.track)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("searchQueries = %q, want %q", got, tc.want)
			}
		})
	}
}
