package events

import "os"

// Writer identifies a source's bounded diagnostic dimensions.
type Writer struct{ Host, Harness, Profile, Source string }

// WriteOutcomes omits writers with no attempted durable append.
func (s *Store) WriteOutcomes() map[Writer]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[Writer]bool, len(s.outcomes))
	for w, ok := range s.outcomes {
		out[w] = ok
	}
	return out
}

// Stats is a current capacity reading, including state and local backups.
type Stats struct {
	Size, Available int64
	Pressure        bool
}

func (s *Store) Stats() (Stats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	size, err := treeSize(s.root)
	if err != nil {
		return Stats{}, err
	}
	free, err := s.free(s.root)
	if err != nil {
		return Stats{}, err
	}
	return Stats{Size: size, Available: free, Pressure: float64(size) >= .8*float64(s.budget) || free < 2*s.reserve}, nil
}

// CheckWritable proves startup can create and fsync data, even in an empty store.
// It does not append a fabricated source event.
func (s *Store) CheckWritable() error {
	f, err := os.CreateTemp(s.root, ".write-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write([]byte{0}); err == nil {
		err = s.sync(f)
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return syncDir(s.root, s.sync)
}
