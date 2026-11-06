package app

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/moophat/agent-quota-exporter/internal/config"
	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

func buildEvents(cfg *config.Config, profiles []config.Resolved) (*events.Store, []byte, error) {
	if err := cfg.ValidateEvents(); err != nil {
		return nil, nil, err
	}
	if !cfg.Events.Enabled {
		return nil, nil, nil
	}
	required := false
	for _, p := range profiles {
		required = required || p.Account == ""
	}
	var key []byte
	if cfg.Events.IdentityKeyFile != "" {
		// Resolve the existing key and the nearest existing root ancestor so
		// a not-yet-created store cannot hide containment through a symlink.
		keyPath, err := filepath.EvalSymlinks(cfg.Events.IdentityKeyFile)
		if err != nil {
			return nil, nil, fmt.Errorf("events.identity_key_file: cannot resolve key file")
		}
		root := filepath.Clean(cfg.Events.RootDir)
		var suffix []string
		for {
			resolved, resolveErr := filepath.EvalSymlinks(root)
			if resolveErr == nil {
				root = filepath.Join(append([]string{resolved}, suffix...)...)
				break
			}
			if filepath.Dir(root) == root {
				return nil, nil, fmt.Errorf("events.root_dir: cannot resolve store root")
			}
			suffix = append([]string{filepath.Base(root)}, suffix...)
			root = filepath.Dir(root)
		}
		rel, err := filepath.Rel(root, keyPath)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return nil, nil, fmt.Errorf("events.identity_key_file: must be outside event store and its backups")
		}
		f, err := os.OpenFile(cfg.Events.IdentityKeyFile, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("events.identity_key_file: cannot open restricted key file")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, nil, fmt.Errorf("events.identity_key_file: requires a regular mode 0600 file")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != uint32(os.Geteuid()) {
			return nil, nil, fmt.Errorf("events.identity_key_file: must be owned by exporter user")
		}
		key, err = io.ReadAll(io.LimitReader(f, 4097))
		if err != nil || len(key) < 32 || len(key) > 4096 {
			return nil, nil, fmt.Errorf("events.identity_key_file: requires 32 to 4096 raw key bytes")
		}
	} else if required {
		return nil, nil, fmt.Errorf("events.identity_key_file: required for profiles without an account alias")
	}
	s, err := events.Open(events.Options{Root: cfg.Events.RootDir, Budget: cfg.Events.ByteBudget, Reserve: cfg.Events.FreeSpaceReserve})
	if err != nil {
		return nil, nil, fmt.Errorf("events: %w", err)
	}
	if err := s.CheckWritable(); err != nil {
		s.Close()
		return nil, nil, fmt.Errorf("events: startup write check: %w", err)
	}
	return s, key, nil
}

// EventSamples measures the sink independently of source cache freshness. Unknown
// source backlog and write outcomes remain absent until adapters report them.
func (b Built) EventSamples() []core.Sample {
	if b.Events == nil {
		return nil
	}
	st, err := b.Events.Stats()
	pressure := 0.0
	if st.Pressure || err != nil {
		pressure = 1
	}
	out := []core.Sample{}
	if err == nil {
		out = append(out,
			core.Sample{Name: "agent_event_store_size_bytes", Value: float64(st.Size)},
			core.Sample{Name: "agent_event_store_available_bytes", Value: float64(st.Available)},
		)
	}
	out = append(out, core.Sample{Name: "agent_event_store_pressure", Value: pressure})
	for i := range out {
		out[i].Labels = []core.Label{{Name: "host", Value: b.host}}
	}
	for w, ok := range b.Events.WriteOutcomes() {
		v := 0.0
		if ok {
			v = 1
		}
		out = append(out, core.Sample{Name: "agent_event_store_write_success", Value: v,
			Labels: []core.Label{{Name: "host", Value: w.Host}, {Name: "harness", Value: w.Harness},
				{Name: "profile", Value: w.Profile}, {Name: "source", Value: w.Source}}})
	}
	return out
}
