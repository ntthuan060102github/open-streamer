package publisher

import (
	"fmt"
	"sync"
)

type portPool struct {
	min, max int
	used     map[int]struct{}
	mu       sync.Mutex
}

func newPortPool(portMin, portMax int) *portPool {
	if portMin <= 0 || portMax < portMin {
		portMin, portMax = 18554, 18554
	}
	return &portPool{min: portMin, max: portMax, used: make(map[int]struct{})}
}

func (p *portPool) alloc() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.min; port <= p.max; port++ {
		if _, ok := p.used[port]; !ok {
			p.used[port] = struct{}{}
			return port, nil
		}
	}
	return 0, fmt.Errorf("publisher: no free TCP port in range %d-%d", p.min, p.max)
}

func (p *portPool) release(port int) {
	if port <= 0 {
		return
	}
	p.mu.Lock()
	delete(p.used, port)
	p.mu.Unlock()
}
