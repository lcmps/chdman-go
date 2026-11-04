package cdrom

import (
	"bytes"
	"errors"
	"testing"
)

func TestWriteCueEntry(t *testing.T) {
	tests := []struct {
		name            string
		num             int
		track           Track
		bin             string
		discOfs, fileOf uint32
		want            string
	}{
		{
			name: "first track of a file", num: 1, bin: "a.bin",
			track: Track{Type: MODE1_RAW, DataSize: 2352},
			want:  "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n",
		},
		{
			name: "later track of a file has no FILE line", num: 2, bin: "a.bin", discOfs: 40, fileOf: 40 * 2352,
			track: Track{Type: AUDIO, DataSize: 2352},
			want:  "  TRACK 02 AUDIO\n    INDEX 01 00:00:40\n",
		},
		{
			name: "mode 1 cooked", num: 3, bin: "a.bin", fileOf: 1,
			track: Track{Type: MODE1, DataSize: 2048},
			want:  "  TRACK 03 MODE1/2048\n    INDEX 01 00:00:00\n",
		},
		{
			name: "mode 2 variants", num: 1, bin: "b.bin", fileOf: 1,
			track: Track{Type: MODE2_FORM_MIX, DataSize: 2336},
			want:  "  TRACK 01 MODE2/2336\n    INDEX 01 00:00:00\n",
		},
		{
			name: "pregap not in the file", num: 2, bin: "a.bin", discOfs: 75 * 60, fileOf: 1,
			track: Track{Type: AUDIO, DataSize: 2352, Pregap: 150},
			want:  "  TRACK 02 AUDIO\n    PREGAP 00:02:00\n    INDEX 01 01:00:00\n",
		},
		{
			name: "pregap in the file", num: 2, bin: "a.bin", discOfs: 100, fileOf: 1,
			track: Track{Type: AUDIO, DataSize: 2352, Pregap: 10, PregapDataSize: 2352},
			want:  "  TRACK 02 AUDIO\n    INDEX 00 00:01:25\n    INDEX 01 00:01:35\n",
		},
		{
			name: "postgap", num: 1, bin: "a.bin",
			track: Track{Type: MODE1_RAW, DataSize: 2352, Postgap: 75},
			want:  "FILE \"a.bin\" BINARY\n  TRACK 01 MODE1/2352\n    INDEX 01 00:00:00\n    POSTGAP 00:01:00\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b bytes.Buffer
			if err := WriteCueEntry(&b, tt.num, tt.track, tt.bin, tt.discOfs, tt.fileOf); err != nil {
				t.Fatal(err)
			}
			if got := b.String(); got != tt.want {
				t.Errorf("got\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestWriteCueEntryReportsWriteError(t *testing.T) {
	if err := WriteCueEntry(failWriter{}, 1, Track{Type: AUDIO, DataSize: 2352}, "a.bin", 0, 0); err == nil {
		t.Error("expected the write error")
	}
}
