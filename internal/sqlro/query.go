package sqlro

import (
	"database/sql"
	"regexp"
	"strconv"
	"strings"

	"github.com/moophat/agent-quota-exporter/internal/core"
)

const (
	maxText  = 128   // characters kept of any text value
	maxKeys  = 500   // keys per ByKeys call
	maxLimit = 10000 // rows per After call
)

var (
	identRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pathRe  = regexp.MustCompile(`^\$(\.[A-Za-z_][A-Za-z0-9_]*)+$`)
)

// Expr is a selectable column expression: Col or JSON.
type Expr interface{ isExpr() }

// Col is a plain column. Never list a column holding a JSON blob or a secret as a Col.
type Col string

// JSON is one scalar at Path (e.g. "$.tokens.input") inside the JSON text column Col.
type JSON struct{ Col, Path string }

func (Col) isExpr()  {}
func (JSON) isExpr() {}

// Allow maps a table name to the expressions a plugin may select from it.
type Allow map[string][]Expr

// denied is the error for anything outside the allow-list or its bounds; no SQL has run when it is returned.
func denied() error { return core.Fail(core.ReasonOther) }

// After returns cols of up to limit rows whose cursorCol is greater than after, in cursorCol order. cursorCol is
// "rowid" or a listed integer Col.
func (d *DB) After(table, cursorCol string, after int64, cols []Expr, limit int) ([][]any, error) {
	if limit < 1 || limit > maxLimit {
		return nil, denied()
	}
	sel, err := d.selectList(table, cols)
	if err != nil {
		return nil, err
	}
	if err := d.check(table, Col(cursorCol)); err != nil {
		return nil, err
	}
	c := quote(cursorCol)
	return d.rows("SELECT "+sel+" FROM "+quote(table)+" WHERE "+c+" > ? ORDER BY "+c+" LIMIT ?",
		len(cols), after, limit)
}

// ByKeys returns cols of the rows whose keyCol (a listed Col) is one of keys, at most maxKeys of them.
func (d *DB) ByKeys(table, keyCol string, keys []string, cols []Expr) ([][]any, error) {
	if len(keys) > maxKeys {
		return nil, denied()
	}
	sel, err := d.selectList(table, cols)
	if err != nil {
		return nil, err
	}
	if err := d.check(table, Col(keyCol)); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = k
	}
	marks := strings.Repeat("?,", len(keys))
	return d.rows("SELECT "+sel+" FROM "+quote(table)+" WHERE "+quote(keyCol)+" IN ("+marks[:len(marks)-1]+")",
		len(cols), args...)
}

// MaxRowid returns the largest rowid of a listed table, 0 when it is empty.
func (d *DB) MaxRowid(table string) (int64, error) {
	return d.MaxInt(table, "rowid")
}

// MaxInt returns the largest value of a listed integer Col, 0 when the table is empty. A non-integer value
// fails with core.ReasonOther.
func (d *DB) MaxInt(table, col string) (int64, error) {
	if err := d.check(table, Col(col)); err != nil {
		return 0, err
	}
	var v sql.NullInt64
	if err := d.db.QueryRow("SELECT max(" + quote(col) + ") FROM " + quote(table)).Scan(&v); err != nil {
		return 0, reason(err)
	}
	return v.Int64, nil
}

// check allows e only if table and e are listed (rowid is implicit) and every identifier and path is well formed.
func (d *DB) check(table string, e Expr) error {
	list, ok := d.allow[table]
	if !ok || !identRe.MatchString(table) {
		return denied()
	}
	switch v := e.(type) {
	case Col:
		if !identRe.MatchString(string(v)) {
			return denied()
		}
		if v == "rowid" {
			return nil
		}
	case JSON:
		if !identRe.MatchString(v.Col) || !pathRe.MatchString(v.Path) {
			return denied()
		}
	default:
		return denied()
	}
	for _, x := range list {
		if x == e {
			return nil
		}
	}
	return denied()
}

// selectList checks every expression, then renders the select list.
func (d *DB) selectList(table string, cols []Expr) (string, error) {
	if len(cols) == 0 {
		return "", denied()
	}
	out := make([]string, len(cols))
	for i, e := range cols {
		if err := d.check(table, e); err != nil {
			return "", err
		}
		out[i] = render(e)
	}
	return strings.Join(out, ", "), nil
}

// render returns the SQL for an expression already passed by check. Text is capped; a JSON path yields a scalar
// or NULL (object, array, absent path or malformed JSON).
func render(e Expr) string {
	n := strconv.Itoa(maxText)
	switch v := e.(type) {
	case Col:
		c := quote(string(v))
		return "CASE WHEN typeof(" + c + ") IN ('text','blob') THEN substr(" + c + ", 1, " + n + ") ELSE " + c + " END"
	case JSON:
		c, p := quote(v.Col), "'"+v.Path+"'"
		t, x := "json_type("+c+", "+p+")", "json_extract("+c+", "+p+")"
		return "CASE WHEN NOT json_valid(" + c + ") THEN NULL" +
			" WHEN " + t + " IN ('integer','real','true','false','null') THEN " + x +
			" WHEN " + t + " = 'text' THEN substr(" + x + ", 1, " + n + ") END"
	}
	panic("sqlro: unchecked expression") // unreachable: check rejects every other type
}

func quote(ident string) string { return `"` + ident + `"` }

// rows runs a built query and scans n columns per row.
func (d *DB) rows(query string, n int, args ...any) ([][]any, error) {
	rs, err := d.db.Query(query, args...)
	if err != nil {
		return nil, reason(err)
	}
	defer rs.Close()
	var out [][]any
	for rs.Next() {
		row, ptrs := make([]any, n), make([]any, n)
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rs.Scan(ptrs...); err != nil {
			return nil, reason(err)
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		return nil, reason(err)
	}
	return out, nil
}
