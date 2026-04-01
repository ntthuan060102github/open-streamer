package publisher

import (
	"testing"

	"github.com/open-streamer/open-streamer/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestStreamCodeFromLivePath(t *testing.T) {
	t.Parallel()
	code, err := streamCodeFromLivePath("/live/my_stream1")
	require.NoError(t, err)
	require.Equal(t, domain.StreamCode("my_stream1"), code)

	_, err = streamCodeFromLivePath("/other/x")
	require.Error(t, err)

	_, err = streamCodeFromLivePath("/live/")
	require.Error(t, err)
}

func TestStreamCodeFromSRTStreamID(t *testing.T) {
	t.Parallel()
	code, err := streamCodeFromSRTStreamID("live/abc")
	require.NoError(t, err)
	require.Equal(t, domain.StreamCode("abc"), code)

	code2, err := streamCodeFromSRTStreamID("xyz")
	require.NoError(t, err)
	require.Equal(t, domain.StreamCode("xyz"), code2)

	_, err = streamCodeFromSRTStreamID("")
	require.Error(t, err)
}
