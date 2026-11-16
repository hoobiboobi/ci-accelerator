package fleet

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// The hub's tables are created with CREATE TABLE IF NOT EXISTS, never by
// numbered migrations: a hub rolled back to an older release opens the file
// as it is. A column added to a table later would not appear in a file an
// earlier release made, so after the tables are created every column the
// schema declares and the file lacks is added. A column added this way
// must have a DEFAULT (SQLite adds a NOT NULL column only with one).

var (
	createTable = regexp.MustCompile(`(?s)CREATE TABLE IF NOT EXISTS (\w+) \((.*?)\n\);`)
	columnLine  = regexp.MustCompile(`^\s*(\w+)\s+(.+?),?\s*$`)
)

// declaredColumns are the columns of each table in schema: table -> column
// -> its definition.
func declaredColumns(schema string) map[string][]string {
	out := map[string][]string{}
	for _, m := range createTable.FindAllStringSubmatch(schema, -1) {
		for _, line := range strings.Split(m[2], "\n") {
			c := columnLine.FindStringSubmatch(line)
			if c == nil || strings.EqualFold(c[1], "PRIMARY") || strings.EqualFold(c[1], "UNIQUE") ||
				strings.EqualFold(c[1], "FOREIGN") || strings.EqualFold(c[1], "CHECK") {
				continue
			}
			out[m[1]] = append(out[m[1]], c[1]+" "+strings.TrimSuffix(c[2], ","))
		}
	}
	return out
}

// ensureColumns adds the columns schema declares that the file's tables
// lack.
func ensureColumns(ctx context.Context, db *sql.DB, schema string) error {
	for table, cols := range declaredColumns(schema) {
		rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
		if err != nil {
			return err
		}
		have := map[string]bool{}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			have[strings.ToLower(n)] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, def := range cols {
			name := strings.Fields(def)[0]
			if have[strings.ToLower(name)] {
				continue
			}
			if _, err := db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+def); err != nil {
				return fmt.Errorf("adding %s.%s to an older file: %w", table, name, err)
			}
		}
	}
	return nil
}

// createSchema creates the hub's tables and indexes in db, adding the
// columns an older file lacks. An index may name a column added later, so
// the statements run again once the columns are there.
func createSchema(ctx context.Context, db *sql.DB) error {
	all := schema + accessSchema + joinSchema + settingsSchema
	_, first := db.ExecContext(ctx, all)
	if err := ensureColumns(ctx, db, all); err != nil {
		return err
	}
	if first == nil {
		return nil
	}
	_, err := db.ExecContext(ctx, all)
	return err
}
