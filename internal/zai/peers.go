package zai

import (
	"crypto/sha256"
	"log/slog"
	"sync"
)

// Peers is the set of zai profiles of one exporter. Two profiles holding the same key are one account: only the
// one with the smallest alias reads it, the others stay silent. Ownership is recomputed from the keys themselves on
// every call, so it is deterministic across restarts and needs no persisted state. The comparison hashes in memory;
// the hash is never stored or logged.
type Peers struct {
	mu      sync.Mutex
	sources []*Source
	warned  map[[2]string]bool
}

// Add registers a source.
func (p *Peers) Add(s *Source) {
	p.mu.Lock()
	p.sources = append(p.sources, s)
	p.mu.Unlock()
}

// owns reports whether self may read its key (selfKey) now: no readable peer with an equal key has a smaller alias.
// A peer whose key cannot be read is ignored. warn logs one line per pair naming the two aliases only.
func (p *Peers) owns(self *Source, selfKey []byte, warn bool) bool {
	mine := sha256.Sum256(selfKey)
	p.mu.Lock()
	peers := append([]*Source(nil), p.sources...)
	p.mu.Unlock()
	for _, o := range peers {
		if o == self || o.Alias >= self.Alias {
			continue
		}
		k, err := o.readKey()
		if err != nil {
			continue
		}
		same := sha256.Sum256(k.value) == mine
		k.Zero()
		if !same {
			continue
		}
		if warn {
			p.warnOnce(self.Alias, o.Alias)
		}
		return false
	}
	return true
}

func (p *Peers) warnOnce(self, owner string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.warned == nil {
		p.warned = map[[2]string]bool{}
	}
	pair := [2]string{self, owner}
	if p.warned[pair] {
		return
	}
	p.warned[pair] = true
	slog.Warn("zai profile shares its key with another profile and is not polled", "profile", self, "owner", owner)
}
