package config

import (
	"testing"
)

func TestLoadRequiresExplicitDatabaseURL(t *testing.T) {
	t.Setenv("DB_HOST", "db.internal")
	t.Setenv("DB_PORT", "6543")
	t.Setenv("DB_NAME", "metafusion_db")
	t.Setenv("DB_USER", "metafusion")
	t.Setenv("DB_PASSWORD", "test-only-password")
	t.Setenv("DB_SSLMODE", "require")
	t.Setenv("DATABASE_URL", " \t ")
	if got := Load().DatabaseURL; got != "" {
		t.Fatalf("缺少 DATABASE_URL 时不得读取 DB_*，got=%q", got)
	}

	const dsn = "postgres://mf_storage:test-only-password@db.internal:6543/metafusion_db?sslmode=require"
	t.Setenv("DATABASE_URL", " \t"+dsn+"\n")
	if got := Load().DatabaseURL; got != dsn {
		t.Fatalf("DatabaseURL = %q，期望显式配置", got)
	}
}
