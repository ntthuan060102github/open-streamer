package publisher

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/yapingcat/gomedia/go-codec"
	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
	rtmpmedia "github.com/yapingcat/gomedia/go-rtmp"

	"github.com/open-streamer/open-streamer/internal/buffer"
	"github.com/open-streamer/open-streamer/internal/domain"
)

func (s *Service) pushToDestination(ctx context.Context, streamID domain.StreamCode, dest domain.PushDestination) {
	sub, err := s.buf.Subscribe(streamID)
	if err != nil {
		slog.Error("publisher: push subscribe failed", "stream_code", streamID, "url", dest.URL, "err", err)
		return
	}
	defer s.buf.Unsubscribe(streamID, sub)

	slog.Info("publisher: push destination started", "stream_code", streamID, "url", dest.URL)

	retryDelay := time.Duration(dest.RetryTimeoutSec) * time.Second
	if retryDelay <= 0 {
		retryDelay = time.Second
	}

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		if dest.Limit > 0 && attempt >= dest.Limit {
			slog.Warn("publisher: push retry limit reached",
				"stream_code", streamID,
				"url", dest.URL,
				"limit", dest.Limit,
			)
			return
		}
		attempt++

		runCtx := ctx
		cancel := func() {}
		if dest.TimeoutSec > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(dest.TimeoutSec)*time.Second)
		}

		pushErr := s.runSinglePushAttempt(runCtx, dest, sub)
		cancel()
		if pushErr == nil || ctx.Err() != nil {
			return
		}

		slog.Warn("publisher: push failed, retrying",
			"stream_code", streamID,
			"url", dest.URL,
			"attempt", attempt,
			"err", pushErr,
			"delay", retryDelay,
		)

		select {
		case <-ctx.Done():
			return
		case <-time.After(retryDelay):
		}
	}
}

func (s *Service) runSinglePushAttempt(
	ctx context.Context,
	dest domain.PushDestination,
	sub *buffer.Subscriber,
) error {
	u := strings.TrimSpace(strings.ToLower(dest.URL))
	switch {
	case strings.HasPrefix(u, "rtmp://"):
		return s.pushRTMP(ctx, dest.URL, sub)
	default:
		return fmt.Errorf("publisher: native push unsupported for URL scheme (only rtmp://): %s", dest.URL)
	}
}

func (s *Service) pushRTMP(ctx context.Context, url string, sub *buffer.Subscriber) error {
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", rtmpTCPAddrFromURL(url))
	if err != nil {
		return fmt.Errorf("rtmp dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	cli := rtmpmedia.NewRtmpClient(
		rtmpmedia.WithComplexHandshake(),
		rtmpmedia.WithEnablePublish(),
	)
	ready := make(chan struct{})
	var readyOnce sync.Once
	cli.OnStateChange(func(st rtmpmedia.RtmpState) {
		if st == rtmpmedia.STATE_RTMP_PUBLISH_START {
			readyOnce.Do(func() { close(ready) })
		}
	})
	cli.SetOutput(func(b []byte) error {
		_, werr := conn.Write(b)
		return werr
	})

	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 64*1024)
		for {
			if ctx.Err() != nil {
				readErr <- ctx.Err()
				return
			}
			n, rerr := conn.Read(buf)
			if n > 0 {
				if ierr := cli.Input(buf[:n]); ierr != nil {
					readErr <- ierr
					return
				}
			}
			if rerr != nil {
				if rerr == io.EOF {
					readErr <- nil
				} else {
					readErr <- rerr
				}
				return
			}
		}
	}()

	cli.Start(url)

	select {
	case <-ctx.Done():
		return nil
	case err := <-readErr:
		if err != nil {
			return err
		}
		return fmt.Errorf("rtmp: connection closed before publish started")
	case <-ready:
	}

	pr, pw := io.Pipe()
	go func() {
		defer func() { _ = pw.Close() }()
		for {
			select {
			case <-ctx.Done():
				return
			case pkt, ok := <-sub.Recv():
				if !ok {
					return
				}
				if _, werr := pw.Write(pkt); werr != nil {
					return
				}
			}
		}
	}()

	demux := mpeg2.NewTSDemuxer()
	demux.OnFrame = func(cid mpeg2.TS_STREAM_TYPE, frame []byte, pts, dts uint64) {
		pt := uint32(pts)
		dt := uint32(dts)
		switch cid {
		case mpeg2.TS_STREAM_H264:
			_ = cli.WriteVideo(codec.CODECID_VIDEO_H264, frame, pt, dt)
		case mpeg2.TS_STREAM_H265:
			_ = cli.WriteVideo(codec.CODECID_VIDEO_H265, frame, pt, dt)
		case mpeg2.TS_STREAM_AAC:
			_ = cli.WriteAudio(codec.CODECID_AUDIO_AAC, frame, pt, dt)
		case mpeg2.TS_STREAM_AUDIO_MPEG1, mpeg2.TS_STREAM_AUDIO_MPEG2:
			_ = cli.WriteAudio(codec.CODECID_AUDIO_MP3, frame, pt, dt)
		}
	}

	demuxDone := make(chan error, 1)
	go func() {
		demuxDone <- demux.Input(pr)
	}()

	for {
		select {
		case <-ctx.Done():
			_ = pr.Close() //nolint:errcheck // best-effort unblock demux
			return nil
		case err := <-readErr:
			_ = pr.Close() //nolint:errcheck
			return err
		case err := <-demuxDone:
			return err
		}
	}
}

// rtmpTCPAddrFromURL extracts host:port from rtmp://host:port/app/stream (best-effort).
func rtmpTCPAddrFromURL(raw string) string {
	u := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(raw)), "rtmp://")
	if idx := strings.Index(u, "/"); idx >= 0 {
		u = u[:idx]
	}
	if !strings.Contains(u, ":") {
		u += ":1935"
	}
	return u
}
