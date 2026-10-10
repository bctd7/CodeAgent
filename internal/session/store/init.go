package store

import (
	sessql "CodeAgent/internal/session/sql"
	"context"
	"database/sql"
	"fmt"
	"strings"
)

func initEmptyDatabase(ctx context.Context, tx *sql.Tx, version int) error {
	if version != 0 {
		return fmt.Errorf("不支持的空数据库版本 %d", version)
	}
	for _, query := range []string{sessql.CreateTablesSQL, sessql.CreateIndexesSQL, sessql.SetUserVersionSQL} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	return nil
}
func getExistingTables(ctx context.Context, tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, sessql.SelectExistingTablesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tables := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables[name] = true
	}
	return tables, rows.Err()
}

// validateDatabase 只接受本版本的七表结构，绝不迁移、回填或补写索引。
func validateDatabase(ctx context.Context, tx *sql.Tx, version int, tables map[string]bool) error {
	if version != CurrentSchemaVersion {
		return fmt.Errorf("不支持的数据库版本 %d，需要 %d；原数据未迁移", version, CurrentSchemaVersion)
	}
	expected := map[string]bool{}
	for _, group := range []struct{ kind, sql string }{{"table", sessql.CreateTablesSQL}, {"index", sessql.CreateIndexesSQL}} {
		for _, statement := range strings.Split(group.sql, ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			prefix := statement[:strings.Index(statement, "(")]
			name := strings.Fields(strings.SplitN(prefix, "IF NOT EXISTS ", 2)[1])[0]
			if group.kind == "table" {
				expected[name] = true
			}
			var actual string
			if err := tx.QueryRowContext(ctx, sessql.SelectSchemaDefinitionSQL, group.kind, name).Scan(&actual); err != nil {
				return fmt.Errorf("检查结构 %s: %w", name, err)
			}
			normalize := func(v string) string {
				return strings.Join(strings.Fields(strings.ReplaceAll(v, "IF NOT EXISTS ", "")), " ")
			}
			if normalize(actual) != normalize(statement) {
				return fmt.Errorf("数据库结构 %s 与当前契约不符", name)
			}
		}
	}
	if len(tables) != len(expected) {
		return fmt.Errorf("数据库必须恰好包含七张契约表")
	}
	for name := range tables {
		if !expected[name] {
			return fmt.Errorf("未知数据表 %s", name)
		}
	}
	rows, err := tx.QueryContext(ctx, sessql.CheckForeignKeysSQL)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("数据库存在失效外键")
	}
	return rows.Err()
}
