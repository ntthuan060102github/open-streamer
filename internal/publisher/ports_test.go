package publisher

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewPortPoolInvalidRangeFallsBack(t *testing.T) {
	t.Parallel()
	p := newPortPool(0, -1)
	require.Equal(t, 18554, p.min)
	require.Equal(t, 18554, p.max)
}

func TestPortPoolAllocAndRelease(t *testing.T) {
	t.Parallel()
	p := newPortPool(4100, 4102)
	a, err := p.alloc()
	require.NoError(t, err)
	require.Equal(t, 4100, a)
	b, err := p.alloc()
	require.NoError(t, err)
	require.Equal(t, 4101, b)
	c, err := p.alloc()
	require.NoError(t, err)
	require.Equal(t, 4102, c)
	_, err = p.alloc()
	require.Error(t, err)

	p.release(4101)
	d, err := p.alloc()
	require.NoError(t, err)
	require.Equal(t, 4101, d)
}

func TestPortPoolReleaseNonPositiveNoPanic(t *testing.T) {
	t.Parallel()
	p := newPortPool(5000, 5000)
	p.release(0)
	p.release(-1)
}
