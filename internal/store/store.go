package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrConflict 表示请求与链路不变量冲突（编号重复、交接竞态等）。
var ErrConflict = errors.New("conflict")

// ErrForbidden 表示角色 / 职责分离规则不允许该操作。
var ErrForbidden = errors.New("forbidden")

// ErrValidation 表示请求参数不合法。
var ErrValidation = errors.New("validation")

// ErrNotFound 表示目标不存在。
var ErrNotFound = errors.New("not found")

// DB 包装样品链路存储。
type DB struct {
	sql *sql.DB
}

// Open 打开（必要时创建）SQLite 数据库并初始化 schema。
func Open(path string) (*DB, error) {
	dsn := "file:" + path + "?_fk=1&_busy_timeout=5000&_journal_mode=WAL"
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// 所有写操作都显式使用 BEGIN IMMEDIATE；单连接串行化避免写锁互相饿死。
	sqlDB.SetMaxOpenConns(1)
	if _, err := sqlDB.Exec(schemaDDL); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if _, err := sqlDB.Exec(immutableTriggers()); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("create triggers: %w", err)
	}
	return &DB{sql: sqlDB}, nil
}

// Close 关闭底层连接。
func (d *DB) Close() error { return d.sql.Close() }

// now 是服务端统一时钟，便于测试替换。
var now = func() time.Time { return time.Now().UTC() }

func ts() string { return now().Format(time.RFC3339Nano) }

// chainHash 计算一条交接事件的链哈希。
// 哈希内容覆盖事件的全部业务字段与上一事件哈希，任何历史事件被物理改动都会断链。
func chainHash(prev string, fields ...string) string {
	h := sha256.New()
	h.Write([]byte(prev))
	for _, f := range fields {
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// conflictError 把唯一索引冲突包装为 ErrConflict。
func mapExecError(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: %s", ErrConflict, err.Error())
	}
	return err
}
