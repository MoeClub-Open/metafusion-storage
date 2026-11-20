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

func TestInternalEndpointIsNeverUsedAsPublicEndpoint(t *testing.T) {
	t.Setenv("STORAGE_S3_ENDPOINT", "http://rustfs:9000")
	t.Setenv("STORAGE_S3_PUBLIC_ENDPOINT", "")
	if got := Load().S3PublicEndpoint; got != "" {
		t.Fatalf("internal endpoint exposed for signing: %q", got)
	}
}

func TestLoadConcurrencyAndRateLimitDefaultsAndOverrides(t *testing.T) {
	cfg := Load()
	if !cfg.RateLimitConfigured || cfg.RateLimitReadRPM != 180 || cfg.RateLimitWriteRPM != 180 {
		t.Fatalf("unexpected default rate limits: %+v", cfg)
	}
	if cfg.UpstreamMaxIdleConnsPerHost != 64 || cfg.S3MaxIdleConnsPerHost != 128 {
		t.Fatalf("unexpected default idle pool limits: upstream=%d s3=%d", cfg.UpstreamMaxIdleConnsPerHost, cfg.S3MaxIdleConnsPerHost)
	}

	t.Setenv("STORAGE_RATE_LIMIT_READ_RPM", "0")
	t.Setenv("STORAGE_RATE_LIMIT_WRITE_RPM", "2400")
	t.Setenv("STORAGE_UPSTREAM_MAX_IDLE_CONNS_PER_HOST", "256")
	t.Setenv("STORAGE_S3_MAX_IDLE_CONNS_PER_HOST", "512")
	cfg = Load()
	if cfg.RateLimitReadRPM != 0 || cfg.RateLimitWriteRPM != 2400 || cfg.UpstreamMaxIdleConnsPerHost != 256 || cfg.S3MaxIdleConnsPerHost != 512 {
		t.Fatalf("unexpected overridden concurrency config: %+v", cfg)
	}

	t.Setenv("STORAGE_RATE_LIMIT_READ_RPM", "-1")
	t.Setenv("STORAGE_RATE_LIMIT_WRITE_RPM", "invalid")
	cfg = Load()
	if cfg.RateLimitReadRPM != 180 || cfg.RateLimitWriteRPM != 180 {
		t.Fatalf("invalid values should fall back to defaults, got read=%d write=%d", cfg.RateLimitReadRPM, cfg.RateLimitWriteRPM)
	}
}
