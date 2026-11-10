package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"blog-server/internal/api"
	"blog-server/internal/auth"
	"blog-server/internal/config"
	"blog-server/internal/maintenance"
	"blog-server/internal/response"
	"blog-server/internal/search"
	"blog-server/internal/setup"
	"blog-server/internal/store"
	"blog-server/internal/web"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	envPath := flag.String("env", "", "配置文件路径（默认 ./.env）")
	forceSetup := flag.Bool("setup", false, "强制进入初始化配置向导（改配置用）")
	setupPort := flag.String("setup-port", "", "向导监听端口（默认与 PORT 相同）")
	hintsPath := flag.String("install-hints", "", "装机脚本生成的预填值文件（默认读 $INSTALL_HINTS）")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, cfgErr := config.LoadFrom(*envPath)
	needSetup := *forceSetup || cfgErr != nil || len(cfg.Missing()) > 0
	reason := ""
	if cfgErr != nil {
		reason = cfgErr.Error()
	} else if needSetup {
		reason = "缺少配置项：" + strings.Join(cfg.Missing(), "、")
	}

	// 配置齐全但库连不上时也别急着退出：进向导改地址，比 systemd 里反复重启好看得多
	if !needSetup {
		if err := quickPing(ctx, cfg.DatabaseURL); err != nil {
			needSetup = true
			reason = "数据库连不上：" + err.Error()
		}
	}

	if needSetup {
		log.Printf("进入初始化向导：%s", reason)
		port := ""
		if cfg != nil {
			port = cfg.Port
		}
		newCfg, err := setup.Run(ctx, setup.Options{
			EnvPath:   *envPath,
			HintsPath: *hintsPath,
			Port:      port,
			Force:     *forceSetup,
			Reason:    reason,
			SetupPort: *setupPort,
		})
		if err != nil {
			log.Printf("初始化向导退出: %v", err)
			return
		}
		cfg = newCfg
		log.Println("初始化配置已写入，开始启动正式服务")
	}

	// dev 下保留 gin 的调试输出，其它环境关掉
	if cfg.AppEnv != "dev" {
		gin.SetMode(gin.ReleaseMode)
	}

	// 开发期启动即迁移，跑一次就幂等。生产建议拆成独立的 migrate 命令。
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		log.Fatalf("数据库迁移失败: %v", err)
	}
	log.Println("数据库迁移完成")

	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("连接数据库失败: %v", err)
	}
	defer pool.Close()
	log.Println("数据库已连接")

	s3c, err := store.NewS3(cfg)
	if err != nil {
		log.Fatalf("连接 MinIO 失败: %v", err)
	}
	core, err := store.NewS3Core(cfg)
	if err != nil {
		log.Fatalf("初始化分片上传客户端失败: %v", err)
	}
	log.Println("MinIO 已连接")

	// 认证：口令散列 + JWT。第一次启动会按 .env 的 ADMIN_USER/ADMIN_PASSWORD 建管理员。
	authMgr, err := auth.New(pool, cfg)
	if err != nil {
		log.Fatalf("初始化认证失败: %v", err)
	}
	if err := authMgr.Bootstrap(ctx); err != nil {
		log.Fatalf("创建管理员失败: %v", err)
	}

	// 搜索：索引不存在就建，然后把 outbox 里的积压补一轮
	searchCli := search.New(cfg)
	if err := searchCli.Ensure(ctx); err != nil {
		log.Printf("[warn] Meilisearch 索引初始化失败（服务继续跑，搜索不可用）: %v", err)
	}
	worker := &search.Worker{Pool: pool, Cli: searchCli}
	go worker.Run(ctx)
	log.Println("搜索同步任务已启动")

	// 后台清理：回收站过期删除、孤儿对象 GC、僵死上传作废，启动后先跑一轮
	maint := &maintenance.Job{Pool: pool, S3: s3c, Core: core, Bucket: cfg.S3Bucket}
	go maint.Loop(ctx)
	log.Println("清理任务已启动（每 12 小时）")

	r := gin.New()
	r.MaxMultipartMemory = 8 << 20 // 8MB 内存缓冲，更大的落临时文件
	r.Use(gin.Logger(), gin.Recovery(), requestID())

	// 自检：只证明进程活着，不碰下游
	r.GET("/healthz", func(c *gin.Context) {
		response.OK(c, gin.H{"time": time.Now().Format(time.RFC3339)})
	})

	// 就绪：所有下游都通才算 200，任一失败返回 503。
	// nginx 的探活接这个，而不是 /healthz。
	r.GET("/readyz", func(c *gin.Context) {
		checks, ok := readiness(c.Request.Context(), pool, cfg)
		if !ok {
			response.FailData(c, http.StatusServiceUnavailable, response.CodeDependency, "依赖未就绪", checks)
			return
		}
		response.OK(c, checks)
	})

	// 业务路由
	v1 := r.Group("/api/v1")
	deps := api.Deps{
		Pool:          pool,
		S3:            s3c,
		Core:          core,
		Bucket:        cfg.S3Bucket,
		PublicBaseURL: cfg.PublicBaseURL,
		Auth:          authMgr,
		Search:        searchCli,
		Maint:         maint,
	}
	api.Register(v1, deps)

	// 短链别名：Markdown 正文里写的是 /raw/<id>、/s/<id>，不想到处带 /api/v1 前缀，
	// 所以在根路径再挂一份同样的 handler（权限判断完全一样）。
	api.RegisterShortLinks(r, deps)

	// 前端两个单文件页面，都编进二进制，和 API 同源，不需要 CORS：
	// "/" 是公开前台（文章/搜索/公开仓库），"/app" 是后台管理页
	r.GET("/", web.Home)
	r.GET("/app", web.Index)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("监听 :%s（env=%s）", cfg.Port, cfg.AppEnv)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("服务退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("收到退出信号，开始优雅关闭")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("优雅关闭超时: %v", err)
	}
	log.Println("已退出")
}

// quickPing 用 3 秒探一下库，只为了决定"直接启动"还是"进配置向导"。
func quickPing(ctx context.Context, dsn string) error {
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	conn, err := pgx.Connect(c, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return conn.Ping(c)
}

// readiness 依次检查 PG / Meili / MinIO，返回每一项的状态。
func readiness(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) (map[string]string, bool) {
	checks := make(map[string]string, 3)
	allOK := true

	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	if err := pool.Ping(cctx); err != nil {
		checks["postgres"] = "fail: " + err.Error()
		allOK = false
	} else {
		checks["postgres"] = "ok"
	}

	meiliURL := cfg.MeiliHost + "/health"
	meiliHeader := map[string]string{"Authorization": "Bearer " + cfg.MeiliAPIKey}
	if err := httpProbe(cctx, meiliURL, meiliHeader); err != nil {
		checks["meilisearch"] = "fail: " + err.Error()
		allOK = false
	} else {
		checks["meilisearch"] = "ok"
	}

	minioURL := cfg.S3Endpoint + "/minio/health/live"
	if err := httpProbe(cctx, minioURL, nil); err != nil {
		checks["minio"] = "fail: " + err.Error()
		allOK = false
	} else {
		checks["minio"] = "ok"
	}

	return checks, allOK
}

func httpProbe(ctx context.Context, url string, headers map[string]string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s 返回 %d", url, resp.StatusCode)
	}
	return nil
}

// requestID 透传或生成一个请求 ID，贯穿日志与响应，方便排查。
func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-ID")
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		response.SetRequestID(c, id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}
