package zai

import (
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

func TestHelperKey(t *testing.T) {
	k, err := readHelper(holderDir(t, "config.yaml", fixtureBody(t, "chelper-config.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Zero()
	if string(k.value) != "CANARYZAI0000000000000000.CANARYHELPER0000" {
		t.Fatal("wrong key")
	}
}

func TestHelperErrors(t *testing.T) {
	cases := map[string]struct{ body, reason string }{
		"missing api_key": {"plan: glm_coding_plan_global\n", core.ReasonNotFound},
		"empty file":      {"", core.ReasonNotFound},
		"not a string":    {"api_key: [a, b]\n", core.ReasonParse},
		"number":          {"api_key: 12345678901234567890\n", core.ReasonParse},
		"bad yaml":        {"api_key: [\n", core.ReasonParse},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := readHelper(holderDir(t, "config.yaml", tc.body))
			wantReason(t, err, tc.reason)
		})
	}
}
