package push

import "sync"

type changes struct {
	mu    sync.Mutex
	paths map[string]bool
	lost  bool
	ready chan struct{}
}

func newChanges() *changes {
	return &changes{paths: make(map[string]bool), ready: make(chan struct{}, 1)}
}

func (c *changes) add(paths []string, lost bool) {
	if len(paths) == 0 && !lost {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range paths {
		c.paths[p] = true
	}
	c.lost = c.lost || lost
	select {
	case c.ready <- struct{}{}:
	default:
	}
}

func (c *changes) take() (paths []string, lost bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for p := range c.paths {
		paths = append(paths, p)
	}
	clear(c.paths)
	lost, c.lost = c.lost, false
	select {
	case <-c.ready:
	default:
	}
	return paths, lost
}
