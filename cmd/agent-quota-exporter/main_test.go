package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOnceWithQuotaRequiresOnce(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"--once-with-quota"}, &out, &errb); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errb.String(), "requires --once") {
		t.Fatalf("stderr = %q, want a message naming --once", errb.String())
	}
}
