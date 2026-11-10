package store

import (
	"database/sql"
	"embed"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // 注册 "pgx" driver，供 goose 使用
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate 把 migrations/ 下未执行的 SQL 依次跑一遍。
// 用的是 goose，不是 AutoMigrate：DDL 以 SQL 为准，避免 ORM 把部分索引、
// INET、TEXT[]、UNIQUE NULLS NOT DISTINCT 这类 Postgres 特性翻译丢。
func Migrate(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("打开迁移连接失败: %w", err)
	}
	defer db.Close()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("设置 goose dialect 失败: %w", err)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		return fmt.Errorf("执行迁移失败: %w", err)
	}
	return nil
}
