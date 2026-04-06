package dvr

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ntthuan060102github/open-streamer/internal/domain"
)

const indexFileName = "index.json"

// loadIndex reads index.json from segDir.
// Returns nil, nil if the file does not exist (fresh recording).
func loadIndex(segDir string) (*domain.DVRIndex, error) {
	data, err := os.ReadFile(filepath.Join(segDir, indexFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var idx domain.DVRIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	return &idx, nil
}

// saveIndex atomically writes index.json to segDir (write tmp → rename).
func saveIndex(segDir string, idx *domain.DVRIndex) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(segDir, indexFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(segDir, indexFileName))
}

// segmentMeta is the in-memory per-segment info used for playlist writing and retention.
// Not persisted to index.json.
type segmentMeta struct {
	index         int
	wallTime      time.Time
	duration      time.Duration
	size          int64
	discontinuity bool
}

// parsePlaylist reads an existing playlist.m3u8 and reconstructs the in-memory
// segment list. Called on resume to continue appending without losing history.
//
// Parses:
//   - #EXT-X-PROGRAM-DATE-TIME → wallTime (applies to next segment and resets after DISCONTINUITY)
//   - #EXT-X-DISCONTINUITY    → discontinuity flag for next segment
//   - #EXTINF:X.XXX,           → duration
//   - filename (e.g. 000003.ts) → index derived from base name
func parsePlaylist(segDir string) ([]segmentMeta, error) {
	f, err := os.Open(filepath.Join(segDir, "playlist.m3u8"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var (
		segments      []segmentMeta
		pendingWall   time.Time
		pendingDur    time.Duration
		pendingDisc   bool
		hasDur        bool
		needDateTime  = true
	)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
			raw := strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
			if t, err := time.Parse("2006-01-02T15:04:05.000Z", raw); err == nil {
				pendingWall = t
				needDateTime = false
			}

		case line == "#EXT-X-DISCONTINUITY":
			pendingDisc = true
			needDateTime = true // expect a DATE-TIME after discontinuity

		case strings.HasPrefix(line, "#EXTINF:"):
			raw := strings.TrimPrefix(line, "#EXTINF:")
			raw = strings.TrimSuffix(raw, ",")
			if s, err := strconv.ParseFloat(raw, 64); err == nil {
				pendingDur = time.Duration(s * float64(time.Second))
				hasDur = true
			}

		case hasDur && !strings.HasPrefix(line, "#") && line != "":
			// This is the segment filename line.
			base := strings.TrimSuffix(filepath.Base(line), ".ts")
			idx, err := strconv.Atoi(base)
			if err != nil {
				hasDur = false
				pendingDisc = false
				continue
			}
			// Try to get file size from disk.
			var size int64
			if fi, err := os.Stat(filepath.Join(segDir, line)); err == nil {
				size = fi.Size()
			}
			segments = append(segments, segmentMeta{
				index:         idx,
				wallTime:      pendingWall,
				duration:      pendingDur,
				size:          size,
				discontinuity: pendingDisc,
			})
			pendingDisc = false
			hasDur = false
			_ = needDateTime
		}
	}
	return segments, scanner.Err()
}
