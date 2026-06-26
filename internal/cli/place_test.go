package cli

import (
	"reflect"
	"testing"

	"github.com/vlauciani/odino/internal/store"
)

func TestSignificantStopTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// Street-type words and articles are dropped, the salient surname survives.
		{"Viale Carlo Tommaso Odescalchi", []string{"carlo", "tommaso", "odescalchi"}},
		{"Via di Porta Maggiore", []string{"porta", "maggiore"}},
		{"Termini", []string{"termini"}},
		// Tokens shorter than 3 chars are noise and dropped.
		{"Re di Roma", []string{"roma"}},
		// Punctuation is a separator.
		{"Appia/Furio Camillo", []string{"appia", "furio", "camillo"}},
		{"", nil},
	}
	for _, c := range cases {
		got := significantStopTokens(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("significantStopTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestMatchesDirection(t *testing.T) {
	lines := []store.LineDirection{
		{RouteShortName: "716", Headsign: "TEATRO MARCELLO", DirectionID: 0},
		{RouteShortName: "716", Headsign: "BALLARIN", DirectionID: 1},
	}
	cases := []struct {
		dir  string
		want bool
	}{
		{"", true},                // no filter matches anything
		{"0", true},               // by direction_id
		{"1", true},               // by direction_id
		{"2", false},              // unknown direction_id
		{"teatro marcello", true}, // headsign substring, case-insensitive
		{"BALLARIN", true},        // headsign exact, other case
		{"marcello", true},        // partial headsign
		{"laurentina", false},     // headsign not served
	}
	for _, c := range cases {
		if got := matchesDirection(lines, c.dir); got != c.want {
			t.Errorf("matchesDirection(_, %q) = %v, want %v", c.dir, got, c.want)
		}
	}
}

func TestSameStopName(t *testing.T) {
	cluster := []store.Stop{
		{StopID: "1", StopName: "TERMINI (MA-MB-FS)"},
		{StopID: "2", StopName: "TERMINI (MA-MB-FS)"},
	}
	street := []store.Stop{
		{StopID: "1", StopName: "ODESCALCHI/BOMPIANI"},
		{StopID: "2", StopName: "ODESCALCHI/TOSTI"},
	}
	if !sameStopName(cluster) {
		t.Error("sameStopName(cluster) = false, want true")
	}
	if sameStopName(street) {
		t.Error("sameStopName(street) = true, want false")
	}
	if sameStopName(nil) {
		t.Error("sameStopName(nil) = true, want false")
	}
}

func TestDistinctLines(t *testing.T) {
	lines := map[string][]store.LineDirection{
		"1": {{RouteShortName: "716"}, {RouteShortName: "n716"}},
		"2": {{RouteShortName: "716"}},
	}
	got := distinctLines(lines)
	want := []string{"716", "n716"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("distinctLines = %v, want %v", got, want)
	}
}
