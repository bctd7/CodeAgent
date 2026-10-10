package sql

const SelectUserVersionSQL = `PRAGMA user_version`
const SelectExistingTablesSQL = `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`
const SelectSchemaDefinitionSQL = `SELECT sql FROM sqlite_master WHERE type = ? AND name = ?`
const CheckForeignKeysSQL = `PRAGMA foreign_key_check`
