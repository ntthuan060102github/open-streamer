//go:build !linux

package transcoder

// pipe_size_other.go — non-Linux fallback (no-op). F_SETPIPE_SZ is a Linux
// extension; macOS / BSD developer builds use the OS default pipe size,
// which is acceptable for low-bitrate test inputs.

import "io"

func setFFmpegStdinPipeSize(_ io.WriteCloser) {}
