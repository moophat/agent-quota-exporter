package zai

import (
	"bytes"
	"errors"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

// readHelper reads api_key from the z.ai coding helper's ~/.chelper/config.yaml.
func readHelper(dir string) (key, error) {
	b, fp, err := openHolderFile(dir, "config.yaml")
	if err != nil {
		return key{}, err
	}
	defer wipe(b)
	var doc struct {
		APIKey any `yaml:"api_key"`
	}
	if err := yaml.NewDecoder(bytes.NewReader(b)).Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return key{}, core.Fail(core.ReasonParse)
	}
	switch v := doc.APIKey.(type) {
	case nil:
		return key{}, core.Fail(core.ReasonNotFound)
	case string:
		if v == "" {
			return key{}, core.Fail(core.ReasonNotFound)
		}
		return finishKey(v, fp)
	}
	return key{}, core.Fail(core.ReasonParse)
}
