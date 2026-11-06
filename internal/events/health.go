package events

import "os"

// Writer identifies a source's bounded diagnostic dimensions.
type Writer struct{ Host, Harness, Profile, Source string }

// ReportFailure records capture failures that happened before Append was reached.
func (s *Store) ReportFailure(w Writer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outcomes[w] = false
}

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
	// Root, budget, reserve and measurement functions are immutable after Open.
	// Filesystem I/O must not hold the mutex that serializes durable appends.
	size, err := s.size(s.root)
	if err != nil {
		return Stats{}, err
	}
	free, err := s.free(s.root)
	if err != nil {
		return Stats{}, err
	}
	return Stats{Size: size, Available: free, Pressure: float64(size) >= .8*float64(s.budget) || free < 2*s.reserve}, nil
}

func safeWriter(e Envelope) bool {
	for _, value := range []string{e.Host, e.Harness, e.Profile, e.Source} {
		if !valid(value) || leaf([]byte(`"`+value+`"`)) != nil {
			return false
		}
	}
	return true
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
