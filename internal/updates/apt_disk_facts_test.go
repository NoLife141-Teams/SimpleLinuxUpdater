package updates

import (
	"math"
	"strconv"
	"testing"
)

func TestParseAptByteSize(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want int64
	}{
		{name: "bytes", raw: "7 B", want: 7},
		{name: "decimal kilobytes", raw: "1.5 kB", want: 1500},
		{name: "uppercase decimal kilobytes", raw: "2 KB", want: 2000},
		{name: "decimal megabytes", raw: "1.25 MB", want: 1_250_000},
		{name: "decimal gigabytes", raw: "2.1 GB", want: 2_100_000_000},
		{name: "binary kibibytes", raw: "1.5 KiB", want: 1536},
		{name: "binary mebibytes", raw: "2 MiB", want: 2 * 1024 * 1024},
		{name: "binary gibibytes", raw: "3 GiB", want: 3 * 1024 * 1024 * 1024},
		{name: "fraction rounds up", raw: "0.1 B", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseAptByteSize(tt.raw)
			if !ok || got != tt.want {
				t.Fatalf("parseAptByteSize(%q) = %d, %t; want %d, true", tt.raw, got, ok, tt.want)
			}
		})
	}
}

func TestParseAptByteSizeRejectsUnsafeInput(t *testing.T) {
	for _, raw := range []string{
		"1,141 MB",
		"-1 MB",
		"1 TB",
		"MB",
		strconv.FormatUint(uint64(math.MaxInt64)+1, 10) + " GiB",
	} {
		t.Run(raw, func(t *testing.T) {
			if got, ok := parseAptByteSize(raw); ok {
				t.Fatalf("parseAptByteSize(%q) = %d, true; want rejected", raw, got)
			}
		})
	}
}

func TestParseAptDiskFacts(t *testing.T) {
	tests := []struct {
		name        string
		output      string
		wantArchive int64
		wantDelta   int64
	}{
		{
			name: "debian additional space",
			output: "Need to get 527 MB of archives.\n" +
				"After this operation, 1.25 GiB of additional disk space will be used.\n",
			wantArchive: 527_000_000,
			wantDelta:   1_342_177_280,
		},
		{
			name: "ubuntu cached archives and freed space",
			output: "Need to get 0 B/88.4 MB of archives.\n" +
				"After this operation, 64.5 MB disk space will be freed.\n",
			wantArchive: 0,
			wantDelta:   -64_500_000,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseAptDiskFacts(tt.output)
			if !ok || got.ArchiveBytes != tt.wantArchive || got.InstalledDeltaBytes != tt.wantDelta {
				t.Fatalf("parseAptDiskFacts() = %+v, %t; want archive=%d delta=%d", got, ok, tt.wantArchive, tt.wantDelta)
			}
		})
	}
}

func TestParseAptDiskFactsRequiresOneCompleteConsistentPair(t *testing.T) {
	tests := map[string]string{
		"archive only":      "Need to get 12 MB of archives.\n",
		"delta only":        "After this operation, 8 MB of additional disk space will be used.\n",
		"malformed archive": "Need to get 1,141 MB of archives.\nAfter this operation, 8 MB of additional disk space will be used.\n",
		"duplicate archive": "Need to get 12 MB of archives.\nNeed to get 13 MB of archives.\nAfter this operation, 8 MB of additional disk space will be used.\n",
		"duplicate delta":   "Need to get 12 MB of archives.\nAfter this operation, 8 MB of additional disk space will be used.\nAfter this operation, 2 MB disk space will be freed.\n",
	}
	for name, output := range tests {
		t.Run(name, func(t *testing.T) {
			if got, ok := parseAptDiskFacts(output); ok {
				t.Fatalf("parseAptDiskFacts() = %+v, true; want conservative fallback", got)
			}
		})
	}
}
