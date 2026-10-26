package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/MoeclubM/metafusion-storage/internal/audit"
	"github.com/MoeclubM/metafusion-storage/internal/auth"
	"github.com/MoeclubM/metafusion-storage/internal/catalog"
	"github.com/MoeclubM/metafusion-storage/internal/config"
	"github.com/MoeclubM/metafusion-storage/internal/handler"
	"github.com/MoeclubM/metafusion-storage/internal/maintenance"
	"github.com/MoeclubM/metafusion-storage/internal/nettrust"
	"github.com/MoeclubM/metafusion-storage/internal/objects"
	"github.com/MoeclubM/metafusion-storage/internal/store"
	"github.com/MoeclubM/metafusion-storage/internal/upstream"
)

// humanBytes / humanDuration 只用于启动日志：0 表示"不限制"，不能打成 0 MB / 0s。
func humanBytes(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return strconv.FormatInt(n>>20, 10) + "MiB"
}

func humanDuration(d time.Duration) string {
	if d <= 0 {
		return "unlimited"
	}
	return d.String()
}

// runWorker 以后台任务模式运行一次：过期上传回收 + 双向对账。退出码 0 表示两项
// 都跑完（明细在日志里）；非 0 表示顶层失败（库/对象不可用），触发器应告警而不是静默跳过。
func runWorker(ctx context.Context, db *store.Store, objs *objects.Store, cfg config.Config) int {
	cleanup, err := maintenance.RunCleanup(ctx, db, objs, cfg)
	if err != nil {
		log.Printf("storage worker: cleanup failed: %v", err)
		return 1
	}
	log.Printf("storage worker: cleanup candidates=%d reclaimed=%d unbound_rows=%d shared_rows=%d already_missing=%d skipped_bound=%d key_mismatch=%d skipped_revoked=%d errors=%d",
		cleanup.Candidates, cleanup.Reclaimed, cleanup.UnboundRowsRemoved, cleanup.SharedRowsRemoved, cleanup.ObjectAlreadyMissing, cleanup.SkippedBound, cleanup.SkippedKeyMismatch, cleanup.SkippedRevoked, cleanup.ObjectErrors)
	recon, err := maintenance.RunReconcile(ctx, db, objs, cfg)
	if err != nil {
		log.Printf("storage worker: reconcile failed: %v", err)
		return 1
	}
	log.Printf("storage worker: reconcile checked=%d missing=%d marked_blocked=%d marking_aborted=%v read_errors=%d orphans_seen=%d quarantined=%d quarantine_errors=%d",
		recon.CheckedComplete, len(recon.MissingAssetIDs), recon.MarkedBlocked, recon.MarkingAborted, recon.ReadErrors, recon.OrphansSeen, recon.OrphansQuarantined, recon.QuarantineErrors)
	return 0
}

// upstreamReadyURL 把上游基址拼成深探针地址；未配置地址返回空串——
// ProbeAll 会把空地址记为 not_configured（那是部署态，不是"上游挂了"），
// 因此这里不需要在调用点写特例。
func upstreamReadyURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	return base + "/ready"
}

func main() {
	cfg := config.Load()
	if cfg.DatabaseURL == "" {
		log.Fatal("DATABASE_URL is required: configure a dedicated storage database identity")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("storage database connection failed: %v", err)
	}
	defer db.Close()
	if err = db.Init(ctx); err != nil {
		log.Fatalf("storage schema initialization failed: %v", err)
	}
	// 审计留痕（契约 docs/architecture/audit-log.md）：记录器自带后台 goroutine 与有界队列；
	// 它必须在 db.Close() 之前关（defer 是后进先出，这里的 defer 排在 db.Close 之后注册即先执行），
	// 否则退出时队列里最后几行会写到已关闭的库上。
	recorder := audit.NewRecorder(db.DB(), audit.ServiceName)
	defer func() {
		recorder.Close()
		if dropped := recorder.Dropped(); dropped > 0 {
			log.Printf("storage: 审计留痕丢弃 %d 行（队列满或写库失败），见 audit_log 的缺口", dropped)
		}
	}()

	objs, err := objects.New(ctx, cfg)
	if err != nil {
		log.Fatalf("object storage initialization failed: %v", err)
	}
	if objs.Local() {
		log.Print("STORAGE_S3_ENDPOINT is unset; using the local object store under " + cfg.Root)
	}
	// 回读校验的上限必须能被运维看见：complete 的大文件失败排查第一步就是确认这两个值，
	// 只写在环境变量里的话，"为什么这份对象被判 hash_verify_too_large"要靠猜。
	log.Printf("upload hash verification: max object size %s, read-back timeout %s",
		humanBytes(objs.VerifyMaxBytes()), humanDuration(objs.VerifyTimeout()))
	// worker 模式：同一个可执行程序的后台任务入口（清理 + 对账），跑一次就退出，
	// 由 cron/systemd timer 周期触发。不是新服务，不开端口、不注册路由。
	if len(os.Args) > 1 && os.Args[1] == "worker" {
		if code := runWorker(ctx, db, objs, cfg); code != 0 {
			os.Exit(code)
		}
		return
	}

	cat := catalog.New(cfg.CatalogURL)
	verifier, err := auth.New(cfg)
	if err != nil {
		log.Fatalf("token verifier initialization failed: %v", err)
	}
	// 跨服务出站都走 upstream 执行器（超时分层 + 有界重试 + 熔断，见 internal/upstream）：
	// 目录可见性、PAT 内省各建一个、长期复用——连接池与熔断器都在实例里，
	// 每次请求新建等于每请求一个熔断器（永远闭合，等于没熔断）。
	// 未配置 AUTH_URL 时内省器仍存在但不会被注入，深探针据此记 not_configured。
	patIntrospector := auth.NewPATIntrospector(cfg.AuthURL)
	if cfg.AuthURL != "" {
		// PAT（mfp_ 前缀）走内省端点
		// POST /api/auth/tokens/introspect，结果进程内缓存 60 秒（= 吊销窗口），见 internal/auth/pat.go。
		verifier.SetPAT(patIntrospector)
		verifier.SetCredentialsChecker(auth.NewCredentialsChecker(cfg.AuthURL))
	} else {
		log.Print("AUTH_URL is not configured: personal access tokens (mfp_ prefix) will be rejected with 503 auth_unavailable")
	}
	// 启动时把实际生效的出站参数打出来：调用点只写自己关心的字段，其余由策略兜底收敛，
	// 排查"为什么这次调用退避了 5 次"时不该靠读代码。
	catPolicy := cat.Upstream().Policy()
	authOutPolicy := patIntrospector.Upstream().Policy()
	log.Printf("upstream policies: catalog(attempts=%d attempt=%s budget=%s breaker=%d/%s) auth(attempts=%d attempt=%s budget=%s breaker=%d/%s)",
		catPolicy.Attempts, catPolicy.AttemptTimeout, catPolicy.Budget, catPolicy.BreakerThreshold, catPolicy.BreakerOpenFor,
		authOutPolicy.Attempts, authOutPolicy.AttemptTimeout, authOutPolicy.Budget,
		authOutPolicy.BreakerThreshold, authOutPolicy.BreakerOpenFor)
	// 深探针目标：地址来自配置（未配置即 not_configured），执行器与请求路径共用。
	upstreamProbes := []upstream.ProbeTarget{
		{Client: cat.Upstream(), URL: upstreamReadyURL(cfg.CatalogURL)},
		{Client: patIntrospector.Upstream(), URL: upstreamReadyURL(cfg.AuthURL)},
	}

	r := gin.New()
	r.Use(gin.Logger(), gin.Recovery())
	// 真实客户端 IP：只信任显式声明的来源（TRUSTED_PROXIES，默认回环 + RFC1918 私网 = 网关容器所在网段）。
	// 此前是 SetTrustedProxies(nil)（谁都不是代理），XFF 被整段忽略、ClientIP() 恒等于网关容器 IP，
	// 审计行的 actor_ip 因此全是网关地址——出事追不到人。配置非法直接拒绝启动：静默退回
	// "无可信代理"会让 IP 记录重新退化成网关地址，而这种退化在功能上表现正常，没人会注意到。
	trustedProxies, perr := nettrust.Apply(r, cfg.TrustedProxies)
	if perr != nil {
		log.Fatalf("trusted proxies configuration invalid: %v", perr)
	}
	log.Printf("trusted proxies for X-Forwarded-For: %s", trustedProxies)
	r.Use(func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		// 切流自检用：响应头标明是哪个服务答复的，便于确认网关把前缀切到了目标上游。
		c.Header("X-MetaFusion-Service", "metafusion-storage")
		c.Next()
	})

	handler.New(db, objs, cat, verifier, cfg).UseAudit(recorder).Register(r)

	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "live", "service": "metafusion-storage"})
	})
	// /ready 是两级探针：浅探针保持"只探 PG、毫秒级"（编排按固定间隔打它，不能被上游拖慢，
	// 也不能因为上游抖动就把一个还能正常读写的实例摘掉）；deep=1 才并发探上游，
	// 回答的是"依赖全绿吗"，用于人工排障与深探监测。
	r.GET("/ready", func(c *gin.Context) {
		check, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := db.DB().PingContext(check); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "unavailable"})
			return
		}
		if c.Query("deep") != "1" {
			c.JSON(http.StatusOK, gin.H{"status": "ready"})
			return
		}
		// 总预算 3s 由 ProbeAll 兜住：深探针不能被测不通的上游拖成慢探针。
		// 未配置地址的目标记为 not_configured（部署态）——它仍是"这一个上游不 ready"，
		// 因此与真正的不可用一样让深探回 degraded，让"没配"和"配了但挂了"都不会被漏看。
		results := upstream.ProbeAll(c.Request.Context(), 3*time.Second, upstreamProbes)
		status, code := "ready", http.StatusOK
		for _, res := range results {
			if res.Status != upstream.ProbeReady {
				status, code = "degraded", http.StatusServiceUnavailable
				break
			}
		}
		c.JSON(code, gin.H{"status": status, "upstreams": results})
	})

	// 刻意不设 ReadTimeout/WriteTimeout：大文件直传与本地对象模式下的整份下发
	// 都可能远超 30 秒，全局写超时会直接截断慢速上传/下载（单体当前的 30s 限制即此因）。
	// 需要限制时按路由加超时，而不是给整个服务设一个会把长传输切掉的全局值。
	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	done := make(chan error, 1)
	go func() {
		log.Print("MetaFusion storage service ready")
		done <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Fatalf("server shutdown error: %v", err)
		}
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		log.Fatalf("server error: %v", err)
	}
}
