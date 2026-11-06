package events

import (
	"os"
	"path/filepath"
)

// Sync every ancestor, including existing directories: a prior failed creation
// may have left a visible but not yet durable directory entry.
func (s *Store) mkdir(path string) error {
	if err := os.MkdirAll(path, 0750); err != nil {
		return err
	}
	for p := path; ; p = filepath.Dir(p) {
		if err := syncDir(p, s.sync); err != nil {
			return err
		}
		if p == filepath.Dir(s.root) || p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
func (s *Store) reconcile() error {
	for path := range s.pending {
		if offset, ok := s.rollback[path]; ok {
			if err := os.Truncate(path, offset); err != nil {
				return err
			}
			delete(s.rollback, path)
		}
		if err := healTail(path); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = s.sync(f)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = s.mkdir(filepath.Dir(path)); err != nil {
			return err
		}
		if err := s.indexShard(path); err != nil {
			return err
		}
		delete(s.pending, path)
	}
	return nil
}
