package zai

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/moophat/agent-quota-exporter/internal/core"
	"github.com/moophat/agent-quota-exporter/internal/sqlro"
)

// opencodeIDs are the integration ids the z.ai helper and opencode use for the Coding Plan, in preference order.
var opencodeIDs = []string{"zai-coding-plan", "zhipuai-coding-plan", "zai", "zhipuai"}

// keyExpr reads the key scalar uncut, plus one character so a value over the 512-byte limit is seen as such.
var keyExpr = sqlro.JSONMax{Col: "value", Path: "$.key", Max: 513}

// credentialAllow is the one deliberate exception to "never read credentials" in opencode's database: the
// credential table, five scalar expressions, never the whole value column.
var credentialAllow = sqlro.Allow{"credential": {
	sqlro.Col("integration_id"), sqlro.Col("active"), sqlro.Col("time_updated"),
	sqlro.JSON{Col: "value", Path: "$.type"}, keyExpr,
}}

// readOpencode looks in the legacy auth.json first, then in the credential table of opencode.db.
func readOpencode(dir string) (key, error) {
	k, authErr := readOpencodeAuth(dir)
	if authErr == nil {
		return k, nil
	}
	k, err := readOpencodeDB(dir)
	if err == nil {
		return k, nil
	}
	// A broken auth.json is more telling than an absent database.
	if reasonOf(authErr) != core.ReasonNotFound && reasonOf(err) == core.ReasonNotFound {
		return key{}, authErr
	}
	return key{}, err
}

func reasonOf(err error) string {
	var se *core.SourceError
	if errors.As(err, &se) {
		return se.Reason
	}
	return core.ReasonOther
}

func readOpencodeAuth(dir string) (key, error) {
	b, fp, err := openHolderFile(dir, "auth.json")
	if err != nil {
		return key{}, err
	}
	defer wipe(b)
	var doc map[string]struct {
		Type string          `json:"type"`
		Key  json.RawMessage `json:"key"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return key{}, core.Fail(core.ReasonParse)
	}
	// "id" and "id/" are one integration: the exact id wins, else the smallest raw id, never map order.
	raw := make([]string, 0, len(doc))
	for id := range doc {
		raw = append(raw, id)
	}
	sort.Strings(raw)
	byID := map[string]json.RawMessage{}
	exact := map[string]bool{}
	for _, id := range raw {
		e, norm := doc[id], strings.TrimRight(id, "/")
		if e.Type != "api" || exact[norm] {
			continue
		}
		if _, taken := byID[norm]; taken && id != norm {
			continue
		}
		byID[norm], exact[norm] = e.Key, id == norm
	}
	for _, id := range opencodeIDs {
		raw, ok := byID[id]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return key{}, core.Fail(core.ReasonParse)
		}
		if s != "" {
			return finishKey(s, fp)
		}
	}
	return key{}, core.Fail(core.ReasonNotFound)
}

func readOpencodeDB(dir string) (key, error) {
	db, err := sqlro.Open(filepath.Join(dir, "opencode.db"), credentialAllow)
	if errors.Is(err, sqlro.ErrIdle) {
		return key{}, core.Fail(core.ReasonOther)
	}
	if err != nil {
		return key{}, err
	}
	defer db.Close()
	rows, err := db.ByKeys("credential", "integration_id", opencodeIDs, []sqlro.Expr{
		sqlro.Col("integration_id"), sqlro.Col("active"), sqlro.Col("time_updated"),
		sqlro.JSON{Col: "value", Path: "$.type"}, keyExpr,
	})
	if err != nil {
		if reasonOf(err) == core.ReasonOther {
			return key{}, core.Fail(core.ReasonNotFound) // an opencode without the credential table
		}
		return key{}, err
	}
	var best struct {
		rank    int
		active  bool
		updated int64
		value   string
		id      string
		ok      bool
	}
	for _, r := range rows {
		id, _ := r[0].(string)
		typ, _ := r[3].(string)
		val, _ := r[4].(string)
		if typ != "key" || val == "" {
			continue
		}
		rank := indexOf(opencodeIDs, id)
		active := toInt(r[1]) == 1
		updated := toInt(r[2])
		better := !best.ok ||
			rank < best.rank ||
			(rank == best.rank && ((active && !best.active) || (active == best.active && updated > best.updated)))
		if better {
			best.rank, best.active, best.updated, best.value, best.id, best.ok = rank, active, updated, val, id, true
		}
	}
	if !best.ok {
		return key{}, core.Fail(core.ReasonNotFound)
	}
	return finishKey(best.value, fmt.Sprintf("db:%d:%s", best.updated, best.id))
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return len(list)
}

func toInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}
