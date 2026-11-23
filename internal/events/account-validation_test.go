package events

import (
	"encoding/json"
	"testing"
)

func TestAccountSecretDetection(t *testing.T) {
	for _, text := range []string{"Bearer invented", "api_key=invented", "access_token:invented", "refresh_token=invented", "sk-000000000000000000000000", "cookie:invented", "password=invented", "-----BEGIN invented"} {
		raw, _ := json.Marshal(text)
		if accountLeaf(raw) == nil {
			t.Errorf("accepted credential marker %q", text)
		}
	}
	for _, text := range []string{"risk-ops@example.com", "access_token-team", "acct-a"} {
		raw, _ := json.Marshal(text)
		if err := accountLeaf(raw); err != nil {
			t.Errorf("rejected account %q: %v", text, err)
		}
	}
	raw, _ := json.Marshal("risk-ops@example.com")
	if leaf(raw) == nil || modelLeaf(raw) == nil {
		t.Fatal("non-account validation weakened")
	}
}
