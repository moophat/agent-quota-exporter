package config

import "testing"

func TestStrataConfig(t *testing.T) {
	c := Default()
	if len(c.Strata) != 0 {
		t.Fatal("enabled by default")
	}
	if err := decode([]byte("strata:\n  - name: lane-a\n    base_url: http://127.0.0.1:8080\n"), c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Strata[0].PollInterval().Seconds() != 30 {
		t.Fatal("wrong default")
	}
	c.Strata[0].Interval = Duration(4e9)
	if c.Validate() == nil {
		t.Fatal("accepted interval below floor")
	}
	c.Strata[0].Interval = Duration(5e9)
	c.Strata[0].BaseURL = "http://user:password@example.com"
	if c.Validate() == nil {
		t.Fatal("accepted credentials")
	}
	for _, suffix := range []string{"?", "#"} {
		c.Strata[0].BaseURL = "http://example.com" + suffix
		if c.Validate() == nil {
			t.Fatal("accepted empty query/fragment delimiter")
		}
	}
}

func TestLaneLabelsReservedOnlyWithStrata(t *testing.T) {
	c := Default()
	c.Labels = map[string]string{"lane": "a", "images": "b", "context": "c"}
	if err := c.Validate(); err != nil {
		t.Fatalf("static lane-named labels without strata must stay valid: %v", err)
	}
	c.Strata = []Lane{{Name: "lane-a", BaseURL: "http://127.0.0.1:8080"}}
	if err := c.Validate(); err == nil {
		t.Fatal("static lane label must collide once strata is configured")
	}
}
