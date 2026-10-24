// Package database provides the shared GORM connection foundation.
package database

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const MySQLURLEnv = "CODEX_MYSQL_URL"

// BaseModel provides an ID, timestamps and GORM soft deletion for future models.
type BaseModel struct {
	gorm.Model
}

// OpenFromEnv opens the database configured by CODEX_MYSQL_URL.
// The caller owns the returned connection and must call Close when finished.
func OpenFromEnv(ctx context.Context) (*gorm.DB, error) {
	return Open(ctx, os.Getenv(MySQLURLEnv))
}

// Open accepts a mysql://user:password@host:port/database URL.
// It verifies connectivity but does not create or migrate application tables.
func Open(ctx context.Context, rawURL string) (*gorm.DB, error) {
	cfg, err := parseURL(rawURL)
	if err != nil {
		return nil, err
	}
	connector, err := mysqldriver.NewConnector(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL connection options")
	}
	sqlDB := sql.OpenDB(connector)
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(3 * time.Minute)
	// 使用调用方的 context 检查连接；失败时关闭连接池，避免资源泄漏。
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping MySQL: %w", err)
	}
	db, err := gorm.Open(mysql.New(mysql.Config{
		Conn:                      sqlDB,
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("initialize GORM: %w", err)
	}
	return db, nil
}

// Close releases the underlying connection pool after all database work finishes.
func Close(db *gorm.DB) error {
	if db == nil {
		return nil
	}
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("get SQL connection: %w", err)
	}
	return sqlDB.Close()
}

func parseURL(rawURL string) (*mysqldriver.Config, error) {
	if strings.TrimSpace(rawURL) == "" {
		return nil, fmt.Errorf("%s is required", MySQLURLEnv)
	}
	u, err := url.Parse(rawURL)
	// URL 解析错误可能包含密码，因此不回传原始 URL 或解析错误。
	if err != nil || u.Scheme != "mysql" || u.Hostname() == "" || u.User == nil || u.User.Username() == "" || u.Fragment != "" || u.Opaque != "" {
		return nil, fmt.Errorf("invalid MySQL URL: expected mysql://user:password@host:port/database")
	}
	name := strings.TrimPrefix(u.Path, "/")
	if name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("MySQL URL must specify one database name")
	}
	params, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL URL query")
	}
	cfg := mysqldriver.NewConfig()
	cfg.User = u.User.Username()
	cfg.Passwd, _ = u.User.Password()
	cfg.Net = "tcp"
	port := u.Port()
	if port == "" {
		port = "3306"
	}
	cfg.Addr = net.JoinHostPort(u.Hostname(), port)
	cfg.DBName = name
	cfg.Timeout = 5 * time.Second
	cfg.Params = map[string]string{"charset": "utf8mb4"}
	for key, values := range params {
		if len(values) != 1 {
			return nil, fmt.Errorf("duplicate MySQL URL query option")
		}
		cfg.Params[key] = values[0]
	}
	// 交给驱动解析 TLS、超时等参数，避免手动拼接 DSN 时破坏转义。
	cfg, err = mysqldriver.ParseDSN(cfg.FormatDSN())
	if err != nil {
		return nil, fmt.Errorf("invalid MySQL connection options")
	}
	// GORM 的时间戳字段依赖驱动将 DATETIME 转换为 time.Time。
	cfg.ParseTime = true
	return cfg, nil
}
