package zai

import (
	"testing"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

func readOpenclawBody(t *testing.T, body string) (key, error) {
	t.Helper()
	return readOpenclaw(holderDir(t, "openclaw.json", body))
}

func TestOpenclawFixture(t *testing.T) {
	k, err := readOpenclawBody(t, fixtureBody(t, "openclaw.json5"))
	if err != nil {
		t.Fatal(err)
	}
	defer k.Zero()
	if string(k.value) != canaryKey {
		t.Fatalf("wrong key chosen")
	}
}

func TestOpenclawSelection(t *testing.T) {
	const a, b, c = "AAAAAAAAAAAAAAAAAAAA", "BBBBBBBBBBBBBBBBBBBB", "CCCCCCCCCCCCCCCCCCCC"
	cases := map[string]struct{ body, want string }{
		"zai wins over zai-anthropic": {`{"models":{"providers":{"zai-anthropic":{"apiKey":"` + b + `"},"zai":{"apiKey":"` + a + `"}}}}`, a},
		"only zai-anthropic":          {`{"models":{"providers":{"zai-anthropic":{"apiKey":"` + b + `"}}}}`, b},
		"env fallback":                {`{"models":{"providers":{"other":{"apiKey":"` + a + `"}}},"env":{"vars":{"ZAI_API_KEY":"` + c + `"}}}`, c},
		"zai beats env":               {`{"models":{"providers":{"zai":{"apiKey":"` + a + `"}}},"env":{"vars":{"ZAI_API_KEY":"` + c + `"}}}`, a},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			k, err := readOpenclawBody(t, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			defer k.Zero()
			if string(k.value) != tc.want {
				t.Fatal("wrong entry chosen")
			}
		})
	}
}

func TestOpenclawErrors(t *testing.T) {
	cases := map[string]struct{ body, reason string }{
		"secret ref object": {`{"models":{"providers":{"zai":{"apiKey":{"source":"env","id":"ZAI"}}}}}`, core.ReasonParse},
		"empty file":        {``, core.ReasonNotFound},
		"whitespace only":   {"  \n// only a comment\n", core.ReasonNotFound},
		"no zai at all":     {`{"models":{"providers":{"other":{"apiKey":"AAAAAAAAAAAAAAAAAAAA"}}}}`, core.ReasonNotFound},
		"number key":        {`{"models":{"providers":{"zai":{"apiKey":12345678901234567890}}}}`, core.ReasonParse},
		"bad shape":         {`{"models":{"providers":{"zai":{"apiKey":"has space in it long enough"}}}}`, core.ReasonParse},
		"single quotes":     {`{'models':{}}`, core.ReasonParse},
		"unquoted keys":     {`{models:{providers:{zai:{apiKey:"AAAAAAAAAAAAAAAAAAAA"}}}}`, core.ReasonParse},
		"malformed":         {`{"models":`, core.ReasonParse},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := readOpenclawBody(t, tc.body)
			wantReason(t, err, tc.reason)
		})
	}
}

func TestOpenclawJSON5Lite(t *testing.T) {
	const a = "AAAAAAAAAAAAAAAAAAAA"
	cases := map[string]string{
		"line and block comments": "// head\n/* b */ {\"models\":/* x */{\"providers\":{\"zai\":{\"apiKey\":\"" + a + "\" // tail\n}}}}\n",
		"trailing commas":         `{"models":{"providers":{"zai":{"apiKey":"` + a + `",},},},}`,
		"trailing comma comment":  "{\"models\":{\"providers\":{\"zai\":{\"apiKey\":\"" + a + "\", // c\n /* d */ }}}}",
		"bom":                     "\xef\xbb\xbf" + `{"models":{"providers":{"zai":{"apiKey":"` + a + `"}}}}`,
		"slashes in strings":      `{"models":{"providers":{"zai":{"baseUrl":"https://api.z.ai/* x */","apiKey":"` + a + `"}}}}`,
		"escaped quote":           `{"x":"a\"//b","models":{"providers":{"zai":{"apiKey":"` + a + `"}}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			k, err := readOpenclawBody(t, body)
			if err != nil {
				t.Fatal(err)
			}
			defer k.Zero()
			if string(k.value) != a {
				t.Fatal("wrong key")
			}
		})
	}
}
