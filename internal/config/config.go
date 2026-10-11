package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是存储服务的运行配置，全部来自环境变量；数据库连接串必须显式配置。
// 对象存储只读取 STORAGE_* 配置。
type Config struct {
	Port string
	// TrustedProxies 是可信反向代理的 IP/CIDR 列表（TRUSTED_PROXIES，逗号分隔）。
	// 只有对端落在列表里时 gin 才采信 X-Forwarded-For，否则 ClientIP() 回退 RemoteAddr；
	// 留空表示用 internal/nettrust 的保守默认（回环 + RFC1918 私网），"none" 表示入口无代理。
	TrustedProxies string
	// DatabaseURL 为必填的 PostgreSQL 连接串，使用存储域独立数据库身份。
	DatabaseURL string
	// Root 本地对象模式的根目录（也用于上传暂存）。
	Root string
	// S3Endpoint 服务端访问对象存储的地址；为空即本地对象模式（无需 RustFS 也能开发）。
	S3Endpoint string
	// S3PublicEndpoint 客户端直传时使用的对外地址：预签名 URL 的 host 参与签名，
	// 因此必须用浏览器可达的地址签发，否则反代后签名校验失败。
	S3PublicEndpoint string
	S3AccessKey      string
	S3SecretKey      string
	S3Bucket         string
	S3TLS            bool
	// JWKSURL 验签公钥来源。auth 服务上线前指向 catalog 的 /api/oidc/jwks，
	// 上线后只改环境变量，不需要改代码。
	JWKSURL string
	// JWTPublicKeyPEM 静态公钥（PEM 或 base64 后的 PEM）；设置后不再请求 JWKS。
	JWTPublicKeyPEM string
	JWTIssuer       string
	JWTAudience     string
	// AuthURL 账号服务地址，用于 PAT 内省及健康探针。会话 JWT 在本地验签。
	AuthURL string
	// CatalogURL 元数据服务地址：实体可见性必须问它，存储侧不复制目录数据。
	CatalogURL string
	// CatalogLifecycleSecret authenticates the internal catalog reference consumer.
	// Empty disables the route; it is separate from all user/session credentials.
	CatalogLifecycleSecret string
	// PresignTTL 预签名有效期；MaxPartCount 单次上传的最大分片数。
	PresignTTL   time.Duration
	MaxPartCount int
	// MaxUploadMB 服务端接收路径（本地对象模式/兜底）的单次上传上限，0 表示不限制。
	// 直传路径的上限由对象存储与网关决定，不受这里影响。
	MaxUploadMB int
	// VerifyMaxMB 是 complete 阶段服务端回读对象、重算 sha256 的**单对象大小上限**：
	// 直传路径的内容没有被服务端逐字节收过，只能在落定前整份读回一次，代价随对象线性增长。
	// 超限即显式失败（hash_verify_too_large），资产留在 pending，绝不"跳过校验但置 complete"。
	// 0 表示不限制——与大文件优先的默认取向一致，需要收敛时由运维按对象存储带宽设定。
	VerifyMaxMB int
	// VerifyTimeout 是同一段回读的墙钟上限：读不完即失败（verify_timeout），同样不跳过校验。
	// 0 表示不限制，与"刻意不设 ReadTimeout/WriteTimeout"的大文件取向一致。
	VerifyTimeout time.Duration
	// PendingTTLHours 为清理策略配置，存量空租约由 000005 迁移按 72 小时回填；
	// 回收判断只依据资产行上的 upload_expires_at。
	PendingTTLHours int
	// UploadLeaseMinutes 是单次上传租约（分钟）：initiate 落定到期时间，续传刷新。
	// 0 表示跟随 PresignTTL——直传地址过期了租约也没意义，两者默认同寿命。
	UploadLeaseMinutes int
	// UserQuotaMB / SiteQuotaMB 是容量预算（MB）：complete 计真实字节，pending 计声明大小。
	// 0 表示不限制（默认）：先有统计口径（/stats 与 UserUsage/SiteUsage），再收紧。
	UserQuotaMB int
	SiteQuotaMB int
	// UserConcurrentUploads / SiteConcurrentUploads 是并发预算：进行中的 pending 数上限。
	// 0 表示不限制；超限时 initiate 回 429 too_many_uploads，容量超限回 413 quota_exceeded。
	UserConcurrentUploads int
	SiteConcurrentUploads int
	// OrphanRetentionDays 是孤儿对象保留期（天）：对象存储上有、库里无引用的键，
	// 首次发现超过该天数才由 worker 搬进 quarantine/ 隔离；隔离≠删除，字节删除由运维确认。
	OrphanRetentionDays int
	// UnboundRetentionDays 是 complete 资产最后解绑后的保留期；重新绑定重新起算。
	UnboundRetentionDays int
	// S3SkipBucketEnsure 是否跳过本服务的建桶检查：默认 false，保持既有行为
	// 置 true 时跳过 BucketExists/MakeBucket，只用仅目标桶数据操作的服务凭据启动（最小权限，见 README）。
	// 管理面（建桶/删桶/跨桶）用另外的运维身份，不与服务凭据共用。
	S3SkipBucketEnsure bool
}

func Load() Config {
	c := Config{
		Port:                   env("PORT", "8082"),
		TrustedProxies:         env("TRUSTED_PROXIES", ""),
		DatabaseURL:            env("DATABASE_URL", ""),
		Root:                   env("STORAGE_ROOT", "./storage-data"),
		S3Endpoint:             env("STORAGE_S3_ENDPOINT", ""),
		S3PublicEndpoint:       env("STORAGE_S3_PUBLIC_ENDPOINT", ""),
		S3AccessKey:            env("STORAGE_S3_ACCESS_KEY", ""),
		S3SecretKey:            env("STORAGE_S3_SECRET_KEY", ""),
		S3Bucket:               env("STORAGE_S3_BUCKET", "metafusion-master"),
		S3TLS:                  envBool("STORAGE_S3_TLS", true),
		JWKSURL:                env("STORAGE_JWKS_URL", "http://auth:8081/api/oidc/jwks"),
		JWTPublicKeyPEM:        env("AUTH_JWT_PUBLIC_KEY", ""),
		JWTIssuer:              env("AUTH_JWT_ISSUER", "https://findverse.cc/api"),
		JWTAudience:            env("AUTH_JWT_AUDIENCE", "metafusion"),
		AuthURL:                env("AUTH_URL", ""),
		CatalogURL:             env("CATALOG_URL", "http://backend:8080"),
		CatalogLifecycleSecret: env("STORAGE_CATALOG_LIFECYCLE_SECRET", ""),
		PresignTTL:             time.Duration(envInt("STORAGE_PRESIGN_TTL_MINUTES", 120)) * time.Minute,
		MaxPartCount:           envInt("STORAGE_MAX_PARTS", 10000),
		MaxUploadMB:            envInt("STORAGE_MAX_UPLOAD_MB", 0),
		VerifyMaxMB:            envInt("STORAGE_VERIFY_MAX_MB", 0),
		VerifyTimeout:          time.Duration(envInt("STORAGE_VERIFY_TIMEOUT_SECONDS", 0)) * time.Second,
		PendingTTLHours:        envInt("STORAGE_PENDING_TTL_HOURS", 72),
		UploadLeaseMinutes:     envInt("STORAGE_UPLOAD_LEASE_MINUTES", 0),
		UserQuotaMB:            envInt("STORAGE_USER_QUOTA_MB", 0),
		SiteQuotaMB:            envInt("STORAGE_SITE_QUOTA_MB", 0),
		UserConcurrentUploads:  envInt("STORAGE_USER_CONCURRENT_UPLOADS", 0),
		SiteConcurrentUploads:  envInt("STORAGE_SITE_CONCURRENT_UPLOADS", 0),
		OrphanRetentionDays:    envInt("STORAGE_ORPHAN_RETENTION_DAYS", 7),
		UnboundRetentionDays:   envInt("STORAGE_UNBOUND_RETENTION_DAYS", 7),
		S3SkipBucketEnsure:     envBool("STORAGE_S3_SKIP_BUCKET_ENSURE", false),
	}
	if c.UploadLeaseMinutes <= 0 {
		c.UploadLeaseMinutes = int(c.PresignTTL.Minutes())
		if c.UploadLeaseMinutes <= 0 {
			c.UploadLeaseMinutes = 120
		}
	}
	if c.PendingTTLHours <= 0 {
		c.PendingTTLHours = 72
	}
	if c.S3PublicEndpoint == "" {
		c.S3PublicEndpoint = c.S3Endpoint
	}
	return c
}

// UploadLease 是单次上传租约时长（Load 已收敛：未配置即跟随预签名有效期）。
func (c Config) UploadLease() time.Duration {
	if c.UploadLeaseMinutes <= 0 {
		return 120 * time.Minute
	}
	return time.Duration(c.UploadLeaseMinutes) * time.Minute
}

// PendingTTL 是历史策略窗口的配置值；回收判断使用资产行上的明确租约。
func (c Config) PendingTTL() time.Duration {
	if c.PendingTTLHours <= 0 {
		return 72 * time.Hour
	}
	return time.Duration(c.PendingTTLHours) * time.Hour
}

// UserQuotaBytes / SiteQuotaBytes 是容量预算的字节数：0 表示不限制。
func (c Config) UserQuotaBytes() int64 { return int64(c.UserQuotaMB) << 20 }
func (c Config) SiteQuotaBytes() int64 { return int64(c.SiteQuotaMB) << 20 }

// OrphanRetention 是孤儿对象保留期。
func (c Config) OrphanRetention() time.Duration {
	if c.OrphanRetentionDays <= 0 {
		return 7 * 24 * time.Hour
	}
	return time.Duration(c.OrphanRetentionDays) * 24 * time.Hour
}

// UnboundRetention 是已完成但失去全部绑定的资产保留期。
func (c Config) UnboundRetention() time.Duration {
	if c.UnboundRetentionDays <= 0 {
		return 7 * 24 * time.Hour
	}
	return time.Duration(c.UnboundRetentionDays) * 24 * time.Hour
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envBool(k string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v != "false" && v != "0"
}

func envInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
