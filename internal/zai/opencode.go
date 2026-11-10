package zai

import "github.com/moophat/agent-quota-exporter/internal/core"

// readOpencode is replaced by the opencode reader.
func readOpencode(dir string) (key, error) { return key{}, core.Fail(core.ReasonNotFound) }
