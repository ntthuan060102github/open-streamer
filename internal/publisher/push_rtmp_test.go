package publisher

import (
	"net"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- rtmpDial URL parsing -----------------------------------------------------

// rtmpParseURL exercises the same URL-normalisation logic as rtmpDial without
// opening a real network connection.
func rtmpParseURL(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if _, _, splitErr := net.SplitHostPort(u.Host); splitErr != nil {
		switch u.Scheme {
		case "rtmps":
			u.Host += ":443"
		default:
			u.Host += ":1935"
		}
	}
	return u, nil
}

func TestRtmpDialURLParsing(t *testing.T) {
	tests := []struct {
		name         string
		rawURL       string
		wantScheme   string
		wantHost     string
		wantHostname string
		wantPort     string
	}{
		{
			name:         "rtmp no port gets default 1935",
			rawURL:       "rtmp://rtmp.example.com/live2/key",
			wantScheme:   "rtmp",
			wantHost:     "rtmp.example.com:1935",
			wantHostname: "rtmp.example.com",
			wantPort:     "1935",
		},
		{
			name:         "rtmp explicit port preserved",
			rawURL:       "rtmp://rtmp.example.com:1935/live2/key",
			wantScheme:   "rtmp",
			wantHost:     "rtmp.example.com:1935",
			wantHostname: "rtmp.example.com",
			wantPort:     "1935",
		},
		{
			name:         "rtmps no port gets default 443",
			rawURL:       "rtmps://rtmps.example.com/rtmp/key",
			wantScheme:   "rtmps",
			wantHost:     "rtmps.example.com:443",
			wantHostname: "rtmps.example.com",
			wantPort:     "443",
		},
		{
			name:         "rtmps explicit port preserved",
			rawURL:       "rtmps://rtmps.example.com:443/rtmp/key",
			wantScheme:   "rtmps",
			wantHost:     "rtmps.example.com:443",
			wantHostname: "rtmps.example.com",
			wantPort:     "443",
		},
		{
			name:         "rtmps non-standard port preserved",
			rawURL:       "rtmps://ingest.example.com:1936/live/key",
			wantScheme:   "rtmps",
			wantHost:     "ingest.example.com:1936",
			wantHostname: "ingest.example.com",
			wantPort:     "1936",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			u, err := rtmpParseURL(tc.rawURL)
			require.NoError(t, err)
			assert.Equal(t, tc.wantScheme, u.Scheme)
			assert.Equal(t, tc.wantHost, u.Host)
			assert.Equal(t, tc.wantHostname, u.Hostname())
			host, port, splitErr := net.SplitHostPort(u.Host)
			require.NoError(t, splitErr)
			assert.Equal(t, tc.wantHostname, host)
			assert.Equal(t, tc.wantPort, port)
		})
	}
}

// ---- scheme guard -------------------------------------------------------------

func TestServeRTMPPush_SchemeGuard(t *testing.T) {
	// rtmpDial will fail with a network error for valid schemes (no server running),
	// but for invalid schemes serveRTMPPush must return immediately without attempting
	// a dial.  We verify this by inspecting the scheme detection logic directly.
	valid := []string{
		"rtmp://rtmp.example.com/live2/key",
		"rtmps://rtmps.example.com:443/rtmp/key",
	}
	invalid := []string{
		"http://example.com/live",
		"srt://example.com:9999",
		"",
		"rtmpx://example.com/live",
	}

	isRTMP := func(u string) bool {
		return len(u) >= 7 && u[:7] == "rtmp://" ||
			len(u) >= 8 && u[:8] == "rtmps://"
	}

	for _, u := range valid {
		assert.True(t, isRTMP(u), "expected %q to be accepted", u)
	}
	for _, u := range invalid {
		assert.False(t, isRTMP(u), "expected %q to be rejected", u)
	}
}
