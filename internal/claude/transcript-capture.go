package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/events"
)

type transcriptCursor struct {
	Device, Inode            uint64
	Offset, Size, Generation int64
	Skipping                 bool
}
type transcriptCapture struct {
	Files                     map[string]*transcriptCursor
	Bytes, Rows, LastComplete int64
	Errors                    int64
}
type transcriptFile struct {
	rel, path string
	mod       time.Time
	size      int64
}

func (s *TranscriptsSource) ReadTimeout() time.Duration {
	if s.Deadline > 0 {
		return s.Deadline
	}
	return 180 * time.Second
}
func (s *TranscriptsSource) capturePath() string {
	return filepath.Join(s.StateDir, "claude-transcripts-"+hashKey(s.P.Host+"\x00"+s.P.Alias)+".json")
}
func (s *TranscriptsSource) saveCapture() error {
	b, err := json.Marshal(s.capture)
	if err != nil {
		return err
	}
	if s.StateDir == "" {
		return s.P.Events.SaveCheckpoint("claude-transcripts:"+s.P.Host+":"+s.P.Alias, b)
	}
	f, err := os.CreateTemp(s.StateDir, ".transcripts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), s.capturePath()); err != nil {
		return err
	}
	d, err := os.Open(s.StateDir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *TranscriptsSource) captureTranscripts(ctx context.Context, now time.Time, res *core.Result) {
	ctx, cancel := context.WithTimeout(ctx, s.ReadTimeout())
	defer cancel()
	if s.capture == nil {
		var b []byte
		var err error
		if s.StateDir != "" {
			b, err = os.ReadFile(s.capturePath())
			if os.IsNotExist(err) {
				err = nil
			}
		} else {
			b, err = s.P.Events.Checkpoint("claude-transcripts:" + s.P.Host + ":" + s.P.Alias)
		}
		s.capture = &transcriptCapture{Files: map[string]*transcriptCursor{}}
		if err != nil || len(b) > 0 && json.Unmarshal(b, s.capture) != nil {
			res.Errors = map[string]int{core.ReasonOther: 1}
			s.capture = nil
			return
		}
		if s.capture.Files == nil {
			s.capture.Files = map[string]*transcriptCursor{}
		}
	}
	c := s.capture
	root := filepath.Join(s.P.Dir, "projects")
	var files []transcriptFile
	var walk func(*os.File, string) error
	walk = func(d *os.File, rel string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		es, err := d.ReadDir(-1)
		if err != nil {
			return err
		}
		for _, e := range es {
			r := filepath.Join(rel, e.Name())
			if e.IsDir() {
				sd, err := openSubdir(d, e.Name())
				if err != nil {
					return err
				}
				err = walk(sd, r)
				sd.Close()
				if err != nil {
					return err
				}
			} else if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
				fi, err := e.Info()
				if err != nil {
					return err
				}
				files = append(files, transcriptFile{r, filepath.Join(root, r), fi.ModTime(), fi.Size()})
			}
		}
		return nil
	}
	var runErr error
	if d, err := openDir(root); err == nil {
		runErr = walk(d, "")
		d.Close()
	} else if !os.IsNotExist(err) {
		runErr = err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].mod.Equal(files[j].mod) {
			return files[i].rel < files[j].rel
		}
		return files[i].mod.After(files[j].mod)
	})
	budget := s.ByteBudget
	if budget <= 0 {
		budget = 256 << 20
	}
	read := s.readBytes
	var rows, skips, resets int64
	readFiles := 0
	for _, file := range files {
		if runErr != nil || ctx.Err() != nil || read >= budget {
			break
		}
		f, fi, err := openRegular(file.path)
		if err != nil {
			runErr = err
			break
		}
		st := fi.Sys().(*syscall.Stat_t)
		k := hashKey(file.rel)
		cur := c.Files[k]
		if cur == nil {
			cur = &transcriptCursor{Device: uint64(st.Dev), Inode: st.Ino}
			c.Files[k] = cur
		}
		if cur.Device != uint64(st.Dev) || cur.Inode != st.Ino || fi.Size() < cur.Size {
			cur.Device = uint64(st.Dev)
			cur.Inode = st.Ino
			cur.Offset = 0
			cur.Skipping = false
			cur.Generation++
			resets++
		}
		cur.Size = fi.Size()
		// Persist generation before emitting anything, including after a replacement.
		if err = s.saveCapture(); err != nil {
			f.Close()
			runErr = err
			break
		}
		if _, err = f.Seek(cur.Offset, io.SeekStart); err != nil {
			f.Close()
			runErr = err
			break
		}
		r := bufio.NewReaderSize(io.LimitReader(f, min(budget-read, fi.Size()-cur.Offset)), 4<<20)
		readFiles++
		var buf []byte
		start := cur.Offset
		consumed := int64(0)
		for ctx.Err() == nil {
			part, e := r.ReadSlice('\n')
			read += int64(len(part))
			consumed += int64(len(part))
			if !cur.Skipping {
				if int64(len(buf)+len(part)) > maxLine {
					cur.Skipping = true
					buf = nil
					skips++
				} else {
					buf = append(buf, part...)
				}
			}
			if e == bufio.ErrBufferFull {
				continue
			}
			if e != nil {
				if e != io.EOF {
					runErr = e
				}
				if cur.Skipping {
					cur.Offset = start + consumed
				}
				break
			}
			end := start + consumed
			if !cur.Skipping {
				raw, id, err := s.transcriptEvent(buf, file.rel, cur, start, end, now)
				if err != nil {
					skips++
				} else if raw != nil {
					exists, err := s.P.Events.Contains(id)
					if err == nil && !exists {
						err = s.P.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}})
						if err == nil {
							rows++
						}
					}
					if err != nil {
						runErr = err
						break
					}
				}
			}
			cur.Offset = end
			cur.Skipping = false
			buf = nil
			start = end
			consumed = 0
		}
		f.Close()
		if err = s.saveCapture(); err != nil {
			runErr = err
		}
	}
	var backlog int64
	for _, f := range files {
		off := int64(0)
		if cur := c.Files[hashKey(f.rel)]; cur != nil {
			off = cur.Offset
		}
		backlog += max(0, f.size-off)
	}
	c.Bytes += read
	c.Rows += rows
	complete := runErr == nil && ctx.Err() == nil && backlog == 0
	if complete {
		c.LastComplete = now.Unix()
	}
	labels := []core.Label{{Name: "host", Value: s.P.Host}, {Name: "harness", Value: Harness}, {Name: "profile", Value: s.P.Alias}, {Name: "source", Value: s.Name()}}
	for name, value := range map[string]int64{"agent_collector_backlog_bytes": backlog, "agent_collector_files_seen": int64(len(files)), "agent_collector_files_read": int64(readFiles), "agent_collector_bytes_read_total": c.Bytes, "agent_collector_rows_appended_total": c.Rows, "agent_collector_last_complete_timestamp_seconds": c.LastComplete} {
		res.Samples = append(res.Samples, core.Sample{Name: name, Value: float64(value), Labels: labels})
	}
	data := map[string]any{"run_id": fmt.Sprint(now.UnixNano()), "started_at": captureTime(now), "ended_at": captureTime(time.Now()), "files_seen": len(files), "files_read": readFiles, "bytes_read": read, "rows_appended": rows, "backlog_bytes": backlog, "skip_count": skips, "reset_count": resets, "budget_exhausted": read >= budget && backlog > 0, "deadline_exhausted": ctx.Err() != nil, "completion": "partial"}
	if complete {
		data["completion"] = "complete"
	}
	if runErr != nil {
		c.Errors++
		errorLabels := append(append([]core.Label{}, labels...), core.Label{Name: "class", Value: "filesystem"})
		res.Samples = append(res.Samples, core.Sample{Name: "agent_collector_errors_total", Value: float64(c.Errors), Labels: errorLabels})
		data["first_error_class"] = "filesystem"
		res.Errors = map[string]int{core.ReasonOther: 1}
	}
	raw, _ := json.Marshal(s.captureEnvelope("collect", hashKey(fmt.Sprint(now.UnixNano())), map[string]any{"type": "probe", "probe_id": fmt.Sprint(now.UnixNano()), "part": "collect"}, captureTime(now), now, data))
	if err := s.P.Events.Append(events.Batch{Envelopes: []json.RawMessage{raw}}); err != nil {
		res.Errors = map[string]int{core.ReasonOther: 1}
	}
	if err := s.saveCapture(); err != nil {
		res.Errors = map[string]int{core.ReasonOther: 1}
		s.capture = nil
	}
}
