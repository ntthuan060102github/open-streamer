package publisher

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-streamer/open-streamer/internal/domain"
)

const publisherRTMPApp = "live"

// publisherLiveMountPath is the URL path for RTSP and the logical path for routing (leading slash).
func publisherLiveMountPath(code domain.StreamCode) string {
	return "/live/" + string(code)
}

func streamCodeFromLivePath(path string) (domain.StreamCode, error) {
	p := strings.TrimSpace(path)
	p = strings.TrimPrefix(p, "/")
	const prefix = publisherRTMPApp + "/"
	if !strings.HasPrefix(p, prefix) {
		return "", fmt.Errorf("path must be /live/<stream_code>")
	}
	raw := strings.TrimPrefix(p, prefix)
	if raw == "" || strings.Contains(raw, "/") {
		return "", fmt.Errorf("invalid stream path")
	}
	code := domain.StreamCode(raw)
	if err := domain.ValidateStreamCode(string(code)); err != nil {
		return "", err
	}
	return code, nil
}

// streamCodeFromSRTStreamID maps SRT handshake streamid to a stream code.
// Accepts "live/<code>" (recommended) or a bare stream code matching ValidateStreamCode.
func streamCodeFromSRTStreamID(streamID string) (domain.StreamCode, error) {
	streamID = strings.TrimSpace(streamID)
	if streamID == "" {
		return "", fmt.Errorf("empty SRT streamid")
	}
	const prefix = publisherRTMPApp + "/"
	if strings.HasPrefix(streamID, prefix) {
		raw := strings.TrimPrefix(streamID, prefix)
		code := domain.StreamCode(raw)
		if err := domain.ValidateStreamCode(string(code)); err != nil {
			return "", err
		}
		return code, nil
	}
	code := domain.StreamCode(streamID)
	if err := domain.ValidateStreamCode(string(code)); err != nil {
		return "", err
	}
	return code, nil
}

func srtCallerURL(host string, port, latencyMS int, code domain.StreamCode) string {
	if latencyMS <= 0 {
		latencyMS = 120
	}
	sid := publisherRTMPApp + "/" + string(code)
	q := url.Values{}
	q.Set("mode", "caller")
	q.Set("transtype", "live")
	q.Set("latency", fmt.Sprintf("%d", latencyMS))
	q.Set("streamid", sid)
	return fmt.Sprintf("srt://%s?%s", net.JoinHostPort(host, strconv.Itoa(port)), q.Encode())
}
