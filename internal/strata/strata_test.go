package strata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestFixturesAndBusy(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "busy"}[busy], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if busy && r.URL.Path == "/slots" {
					w.Write([]byte(`[{"is_processing":true},{"is_processing":false}]`))
					return
				}
				b, err := os.ReadFile("testdata" + r.URL.Path + ".json")
				if err != nil {
					t.Error(err)
					w.WriteHeader(404)
					return
				}
				w.Write(b)
			}))
			defer server.Close()
			s := &Source{Host: "node-a", Lane: "lane-a", BaseURL: server.URL}
			result, _, err := s.Collect(context.Background(), time.Now(), nil)
			if err != nil {
				t.Fatal(err)
			}
			values := map[string]float64{}
			for _, sample := range result.Samples {
				values[sample.Name] = sample.Value
				if len(sample.Labels) < 2 || sample.Labels[1].Value != "lane-a" {
					t.Fatal(sample)
				}
			}
			want := float64(0)
			slots := float64(1)
			if busy {
				want = 1
				slots = 2
			}
			if v, ok := values["agent_lane_busy"]; !ok || v != want {
				t.Fatal(values)
			}
			if values["agent_lane_slots"] != slots || len(values) != 5 || values["agent_lane_info"] != 1 {
				t.Fatal(values)
			}
		})
	}
}

func TestMissingValuesAreAbsent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slots" {
			w.Write([]byte(`[{}]`))
		} else {
			w.Write([]byte(`{}`))
		}
	}))
	defer server.Close()
	s := &Source{BaseURL: server.URL}
	r, _, err := s.Collect(context.Background(), time.Now(), nil)
	if err != nil || len(r.Samples) != 1 || r.Samples[0].Name != "agent_lane_slots" {
		t.Fatalf("%+v %v", r, err)
	}
}
