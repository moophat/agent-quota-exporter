package claude

// AI-Model: t9r/cx/gpt-6.1-sol
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

func transcriptOwner(raw []byte, owner *string) {
	if !bytes.Contains(raw, []byte(`"session-owner"`)) {
		return
	}
	var row struct {
		Type string `json:"type"`
		UUID string `json:"accountUuid"`
	}
	if json.Unmarshal(raw, &row) == nil && row.Type == "session-owner" {
		*owner = row.UUID
	}
}

// Search backwards in bounded blocks, retaining only complete structural lines.
// At most 64 MiB preceding the cursor is examined; older evidence stays unknown.
func recoverTranscriptOwner(ctx context.Context, f *os.File, offset int64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	const capBytes int64 = 64 << 20
	floor := max(int64(0), offset-capBytes)
	end := offset
	var fragments [][]byte
	// The cursor may initially be EOF in the middle of a record. Only newline-
	// terminated owner records can affect subsequent complete messages.
	trailing := true
	for end > floor && ctx.Err() == nil {
		start := max(floor, end-(64<<10))
		block := make([]byte, end-start)
		n, err := f.ReadAt(block, start)
		if err != nil && n != len(block) {
			return "", err
		}
		if trailing {
			if split := bytes.LastIndexByte(block, '\n'); split >= 0 {
				block = block[:split+1]
				trailing = false
			} else {
				end = start
				continue
			}
		}
		if split := bytes.LastIndexByte(block, '\n'); split >= 0 && len(fragments) > 0 {
			line := append([]byte{}, block[split+1:]...)
			for i := len(fragments) - 1; i >= 0; i-- {
				line = append(line, fragments[i]...)
			}
			var owner string
			transcriptOwner(line, &owner)
			if owner != "" {
				return owner, nil
			}
			block = block[:split+1]
			fragments = nil
		}
		for {
			last := bytes.LastIndexByte(block, '\n')
			if last < 0 {
				break
			}
			// A trailing newline terminates the preceding line, not a new record.
			tail := block[last+1:]
			var owner string
			transcriptOwner(tail, &owner)
			if owner != "" {
				return owner, nil
			}
			block = block[:last]
		}
		if len(block) > 0 {
			fragments = append(fragments, block)
		}
		end = start
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if floor == 0 {
		var owner string
		var line []byte
		for i := len(fragments) - 1; i >= 0; i-- {
			line = append(line, fragments[i]...)
		}
		transcriptOwner(line, &owner)
		return owner, nil
	}
	return "", nil
}

// Refresh once before both transcript passes.
func (p *Profile) refreshOwnerSnapshot() {
	samples := p.identityHistory()
	err := p.ResolveAccount()
	label, uuid := p.Account()
	if err != nil {
		uuid = ""
	}
	p.mu.Lock()
	p.ownerHistory, p.ownerSnapshotReady = samples, true
	p.ownerLabel, p.ownerUUID = label, uuid
	p.mu.Unlock()
}

func (s *TranscriptsSource) messageOwner(timestamp, owner string) (metric string, account any, interval any, evidence, quality string) {
	s.P.mu.Lock()
	ready := s.P.ownerSnapshotReady
	s.P.mu.Unlock()
	if !ready {
		s.P.refreshOwnerSnapshot()
	}
	s.P.mu.Lock()
	samples := s.P.ownerHistory
	currentLabel, currentUUID := s.P.ownerLabel, s.P.ownerUUID
	s.P.mu.Unlock()
	if owner != "" {
		label := fmt.Sprintf("acct-%x", sha256.Sum256([]byte(owner)))[:13]
		// The verified current login keeps the profile's own label (alias or email), so its owner-bearing and
		// ownerless usage stay one series.
		if (s.P.AccountAlias != "" || s.P.EmailLabel) && currentUUID != "" && owner == currentUUID {
			label = currentLabel
		}
		return label, label, nil, "session_owner", "embedded"
	}
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err == nil {
		binding := identityAt(samples, at)
		if binding.Account != nil && binding.Quality == "sampled" {
			// MetricAccount belongs to the same historical interval, never today's login.
			metric = "unknown"
			if binding.MetricAccount != nil {
				metric = *binding.MetricAccount
			}
			return metric, *binding.Account, binding.IntervalID, "identity_binding", "sampled"
		}
	}
	return "unknown", nil, nil, "none", "unattributed"
}
