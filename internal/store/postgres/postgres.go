package postgres

import (
	"context"
	"embed"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// DB 实现 store.Store。
type DB struct {
	pool *pgxpool.Pool
}

// Open 建立连接池并校验连通性。
func Open(ctx context.Context, dsn string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("解析 DATABASE_URL 失败: %w", err)
	}
	cfg.MaxConns = 10
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 30 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("创建连接池失败: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Close 关闭连接池。
func (d *DB) Close() { d.pool.Close() }

// Ping 健康检查。
func (d *DB) Ping(ctx context.Context) error { return d.pool.Ping(ctx) }

// Migrate 按文件名顺序执行未应用的迁移。DDL 均为幂等语句，失败即中止启动。
func (d *DB) Migrate(ctx context.Context) error {
	if _, err := d.pool.Exec(ctx,
		`create table if not exists schema_migrations (
			version    text primary key,
			applied_at timestamptz not null default now()
		)`); err != nil {
		return fmt.Errorf("创建 schema_migrations 失败: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := d.pool.QueryRow(ctx,
			`select exists(select 1 from schema_migrations where version = $1)`, name).Scan(&applied); err != nil {
			return err
		}
		if applied {
			continue
		}

		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}

		// 一个文件内可能有多条语句，必须走 simple protocol
		conn, err := d.pool.Acquire(ctx)
		if err != nil {
			return err
		}
		results, err := conn.Conn().PgConn().Exec(ctx, string(body)).ReadAll()
		conn.Release()
		if err != nil {
			return fmt.Errorf("执行迁移 %s 失败: %w", name, err)
		}
		for _, res := range results {
			if res.Err != nil {
				return fmt.Errorf("执行迁移 %s 失败: %w", name, res.Err)
			}
		}

		if _, err := d.pool.Exec(ctx,
			`insert into schema_migrations (version) values ($1) on conflict do nothing`, name); err != nil {
			return err
		}
	}
	return nil
}
