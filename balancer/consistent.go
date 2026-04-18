package balancer

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

const replicas = 150

type ringEntry struct {
	hash    uint32
	backend *Backend
}

type consistentHash struct {
	all  []*Backend
	ring []ringEntry
}

func newConsistentHash(backends []*Backend) *consistentHash {
	c := &consistentHash{all: backends}
	c.buildRing(backends)
	return c
}

func (c *consistentHash) buildRing(backends []*Backend) {
	c.ring = c.ring[:0]
	for _, b := range backends {
		for i := 0; i < replicas; i++ {
			key := fmt.Sprintf("%s#%d", b.Address, i)
			h := hashKey(key)
			c.ring = append(c.ring, ringEntry{hash: h, backend: b})
		}
	}
	sort.Slice(c.ring, func(i, j int) bool {
		return c.ring[i].hash < c.ring[j].hash
	})
}

func (c *consistentHash) Next(key string) *Backend {
	healthy := healthyBackends(c.all)
	if len(healthy) == 0 {
		return nil
	}

	// Build a temporary ring with only healthy backends.
	var ring []ringEntry
	healthySet := make(map[*Backend]bool, len(healthy))
	for _, b := range healthy {
		healthySet[b] = true
	}
	for _, e := range c.ring {
		if healthySet[e.backend] {
			ring = append(ring, e)
		}
	}
	if len(ring) == 0 {
		return healthy[0]
	}

	h := hashKey(key)
	idx := sort.Search(len(ring), func(i int) bool {
		return ring[i].hash >= h
	})
	if idx == len(ring) {
		idx = 0
	}
	return ring[idx].backend
}

func (c *consistentHash) All() []*Backend {
	return c.all
}

func hashKey(key string) uint32 {
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint32(sum[:4])
}
