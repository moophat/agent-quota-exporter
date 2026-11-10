package zai

import (
	"bytes"
	"encoding/json"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// readOpenclaw reads the z.ai key from OpenClaw's openclaw.json. The file is JSON5 in the wild; only comments, a
// BOM and trailing commas are understood, anything else (single quotes, unquoted keys) is a clean parse failure.
func readOpenclaw(dir string) (key, error) {
	b, fp, err := openHolderFile(dir, "openclaw.json")
	if err != nil {
		return key{}, err
	}
	defer wipe(b)
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	clean := stripJSON5Lite(b)
	defer wipe(clean)
	if len(bytes.TrimSpace(clean)) == 0 {
		return key{}, core.Fail(core.ReasonNotFound)
	}
	var doc struct {
		Models struct {
			Providers map[string]struct {
				APIKey json.RawMessage `json:"apiKey"`
			} `json:"providers"`
		} `json:"models"`
		Env struct {
			Vars map[string]json.RawMessage `json:"vars"`
		} `json:"env"`
	}
	if err := json.Unmarshal(clean, &doc); err != nil {
		return key{}, core.Fail(core.ReasonParse)
	}
	candidates := []json.RawMessage{
		doc.Models.Providers["zai"].APIKey,
		doc.Models.Providers["zai-anthropic"].APIKey,
		doc.Env.Vars["ZAI_API_KEY"],
	}
	for _, raw := range candidates {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return key{}, core.Fail(core.ReasonParse) // SecretRef object, number, ...
		}
		if s == "" {
			continue
		}
		return finishKey(s, fp)
	}
	return key{}, core.Fail(core.ReasonNotFound)
}

// stripJSON5Lite removes // and /* */ comments and trailing commas outside double-quoted strings.
func stripJSON5Lite(in []byte) []byte {
	noComments := make([]byte, 0, len(in))
	inStr := false
	for i := 0; i < len(in); i++ {
		c := in[i]
		if inStr {
			noComments = append(noComments, c)
			if c == '\\' && i+1 < len(in) {
				i++
				noComments = append(noComments, in[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			noComments = append(noComments, c)
		case c == '/' && i+1 < len(in) && in[i+1] == '/':
			for i < len(in) && in[i] != '\n' {
				i++
			}
			noComments = append(noComments, '\n')
		case c == '/' && i+1 < len(in) && in[i+1] == '*':
			i += 2
			for i+1 < len(in) && !(in[i] == '*' && in[i+1] == '/') {
				i++
			}
			i++ // lands on the closing '/'; an unterminated comment just ends the input
			noComments = append(noComments, ' ')
		default:
			noComments = append(noComments, c)
		}
	}
	// Second pass: drop a comma whose next significant byte closes an object or array.
	out := make([]byte, 0, len(noComments))
	inStr = false
	for i := 0; i < len(noComments); i++ {
		c := noComments[i]
		if inStr {
			out = append(out, c)
			if c == '\\' && i+1 < len(noComments) {
				i++
				out = append(out, noComments[i])
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
		}
		if c == ',' {
			j := i + 1
			for j < len(noComments) && (noComments[j] == ' ' || noComments[j] == '\t' || noComments[j] == '\n' || noComments[j] == '\r') {
				j++
			}
			if j < len(noComments) && (noComments[j] == '}' || noComments[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	wipe(noComments)
	return out
}
