package publisher

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/grafov/m3u8"
	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

// segmenterOpts configures native MPEG-TS segment files + optional HLS playlist.
type segmenterOpts struct {
	streamDir   string
	hlsPlaylist string // empty: skip m3u8
	segmentSec  int
	window      int
	history     int
	ephemeral   bool
}

func runFilesystemSegmenter(ctx context.Context, streamID domain.StreamCode, sub *buffer.Subscriber, o segmenterOpts) {
	segSec := o.segmentSec
	if segSec <= 0 {
		segSec = 2
	}
	win := o.window
	if win <= 0 {
		win = 12
	}
	hist := o.history
	if hist < 0 {
		hist = 0
	}
	capacity := uint(win + hist + 8)
	wsize := uint(win)
	var playlist *m3u8.MediaPlaylist
	if o.hlsPlaylist != "" {
		var perr error
		playlist, perr = m3u8.NewMediaPlaylist(wsize, capacity)
		if perr != nil {
			slog.Error("publisher: m3u8 playlist init failed", "stream_code", streamID, "err", perr)
			return
		}
		playlist.SetVersion(3)
	}

	var segBuf bytes.Buffer
	segIndex := 1
	segmentStart := time.Now()
	onDisk := make([]string, 0, win+hist+4)
	segDur := float64(segSec)

	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	flush := func() error {
		if segBuf.Len() == 0 {
			segmentStart = time.Now()
			return nil
		}
		name := fmt.Sprintf("seg_%06d.ts", segIndex)
		path := filepath.Join(o.streamDir, name)
		if err := writeFileAtomic(path, segBuf.Bytes()); err != nil {
			return err
		}
		segIndex++
		segBuf.Reset()
		segmentStart = time.Now()

		onDisk = append(onDisk, name)
		maxKeep := win + hist
		if maxKeep < win {
			maxKeep = win
		}
		for o.ephemeral && len(onDisk) > maxKeep {
			old := onDisk[0]
			onDisk = onDisk[1:]
			_ = os.Remove(filepath.Join(o.streamDir, old))
		}

		if o.hlsPlaylist != "" && playlist != nil {
			playlist.Slide(name, segDur, "")
			if err := writeFileAtomic(o.hlsPlaylist, playlist.Encode().Bytes()); err != nil {
				return err
			}
		}
		return nil
	}

	for {
		select {
		case <-ctx.Done():
			_ = flush()
			return
		case <-tick.C:
			if time.Since(segmentStart) >= time.Duration(segSec)*time.Second {
				if err := flush(); err != nil {
					slog.Warn("publisher: segment flush failed", "stream_code", streamID, "err", err)
				}
			}
		case pkt, ok := <-sub.Recv():
			if !ok {
				_ = flush()
				return
			}
			_, _ = segBuf.Write(pkt)
		}
	}
}

func windowTail(names []string, n int) []string {
	if n <= 0 || len(names) == 0 {
		return nil
	}
	start := len(names) - n
	if start < 0 {
		start = 0
	}
	return append([]string(nil), names[start:]...)
}

func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".seg-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
