package store

import (
	"context"
	"database/sql"
	"testing"
)

// 驱动注册是运行期前提：漏掉 sql 驱动的空导入时，编译与静态检查全都通过，
// 但服务一启动就报 unknown driver。这条用例把它变成可离线复现的失败。
func TestPostgresDriverRegistered(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://user:pass@127.0.0.1:1/probe?sslmode=disable")
	if err != nil {
		t.Fatalf("postgres 驱动未注册（检查 lib/pq 的空导入）: %v", err)
	}
	_ = db.Close()
}

func TestOpenValidatesDBPoolEnvVars(t *testing.T) {
	t.Setenv("STORAGE_DB_MAX_OPEN_CONNS", "0")
	if _, err := Open(context.Background(), "postgres://user:pass@127.0.0.1:1/probe?sslmode=disable"); err == nil {
		t.Fatal("expected invalid STORAGE_DB_MAX_OPEN_CONNS=0 to fail")
	}
	t.Setenv("STORAGE_DB_MAX_OPEN_CONNS", "32")
	t.Setenv("STORAGE_DB_MAX_IDLE_CONNS", "64")
	if _, err := Open(context.Background(), "postgres://user:pass@127.0.0.1:1/probe?sslmode=disable"); err == nil {
		t.Fatal("expected STORAGE_DB_MAX_IDLE_CONNS > STORAGE_DB_MAX_OPEN_CONNS to fail")
	}
}
