//go:build linux

package transcoder

// pipe_size_linux.go — bump the FFmpeg stdin pipe buffer past Linux's 64 KiB
// default so the transcoder doesn't backpressure the buffer hub.
//
// Why this matters: the transcoder feeds raw MPEG-TS bytes into FFmpeg via
// `cmd.StdinPipe()`. Linux pipes default to 16 pages × 4 KiB = 64 KiB. At
// 120 Mbps source ingest the pipe fills in ~4 ms — so any FFmpeg input-thread
// scheduling jitter past that window blocks our `stdin.Write` goroutine,
// which stops draining the `$raw$<code>` buffer-hub subscriber, which fills,
// which drops packets via the silent `select default:` fan-out policy. TS
// continuity then breaks and FFmpeg sees corrupted PES headers downstream
// (`reference count overflow`, `decode_slice_header error`, garbled output
// for HLS / DASH / RTMP-push consumers).
//
// 1 MiB gives ~70 ms of slack — enough for normal scheduling + GC pause
// without hitting the buffer-hub drop path.
//
// Linux clamps the request to /proc/sys/fs/pipe-max-size (default 1 MiB on
// most distros). EPERM if request > pipe-max-size and the process lacks
// CAP_SYS_RESOURCE — we silently fall back; whatever the kernel granted is
// strictly better than no attempt.

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const ffmpegStdinPipeSize = 1 << 20 // 1 MiB

// setFFmpegStdinPipeSize bumps the kernel pipe buffer for the writable end
// returned by cmd.StdinPipe(). The returned io.WriteCloser is in fact a
// *os.File — type-assert to access Fd().
func setFFmpegStdinPipeSize(pipe io.WriteCloser) {
	f, ok := pipe.(*os.File)
	if !ok {
		return
	}
	_, _ = unix.FcntlInt(f.Fd(), unix.F_SETPIPE_SZ, ffmpegStdinPipeSize)
}
