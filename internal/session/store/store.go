package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	sessql "CodeAgent/internal/session/sql"

	_ "modernc.org/sqlite"
)

// CurrentSchemaVersion 是当前支持的表结构和消息格式版本。
const CurrentSchemaVersion = 4

// Store 负责底层数据库连接生命周期、配置以及提供可复用的事务执行能力。
type Store struct {
	db *sql.DB
}

// Open 打开 SQLite 数据库并配置单连接与必要 PRAGMA 参数。
// path 按实际文件路径处理，支持中文、空格及特殊字符。Open 仅验证连接，不自动建表或升级版本。
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("数据库文件路径不能为空")
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("解析数据库文件路径: %w", err)
	}
	uriPath := filepath.ToSlash(absPath)
	if filepath.VolumeName(absPath) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	params := url.Values{}
	params.Add("_pragma", "foreign_keys(ON)")
	params.Add("_pragma", "busy_timeout(5000)")
	uri.RawQuery = params.Encode()

	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, fmt.Errorf("打开数据库: %w", err)
	}

	// 单连接池配置，避免读写锁等待与连接交替冲突
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接数据库: %w", err)
	}

	return &Store{db: db}, nil
}

// Close 关闭底层数据库连接。
func (s *Store) Close() error {
	return s.db.Close()
}

// Init 作为统一入口识别空库、当前版本、受支持旧版本和未知／更高版本，再调用对应流程。
func (s *Store) Init(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启数据库初始化事务: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, sessql.SelectUserVersionSQL).Scan(&version); err != nil {
		return fmt.Errorf("读取数据库版本: %w", err)
	}

	if version > CurrentSchemaVersion {
		return fmt.Errorf("数据库版本 %d 高于当前程序支持的版本 %d", version, CurrentSchemaVersion)
	}

	existingTables, err := getExistingTables(ctx, tx)
	if err != nil {
		return err
	}

	if len(existingTables) == 0 {
		if err := initEmptyDatabase(ctx, tx, version); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("提交数据库初始化事务: %w", err)
		}
		return nil
	}

	if err := migrateDatabase(ctx, tx, version, existingTables); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交数据库迁移事务: %w", err)
	}
	return nil
}

// Tx 包装同一个事务对象，提供事务内读写能力，避免单连接自等待。
type Tx struct {
	tx *sql.Tx
}

// WithTx 在同一个数据库写事务内执行操作。如果 fn 返回错误则自动回滚，否则提交。
func (s *Store) WithTx(ctx context.Context, fn func(tx *Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务: %w", err)
	}
	defer tx.Rollback()

	if err := fn(&Tx{tx: tx}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// WithReadTx 在只读事务内执行查询操作。
func (s *Store) WithReadTx(ctx context.Context, fn func(tx *Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("开启只读事务: %w", err)
	}
	defer tx.Rollback()

	return fn(&Tx{tx: tx})
}
