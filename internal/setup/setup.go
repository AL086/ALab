// Package setup 首次运行的配置向导。
//
// 目标：解压、装好依赖之后，直接起二进制就够了 —— 没有 .env 时不报错退出，
// 而是在同端口上开一个向导页，让人填数据库/对象存储/搜索的地址、端口、口令，
// 每个组件都能当场"测试连接"，通过之后才写 .env、跑迁移、建桶、建索引、建管理员，
// 然后把监听交还给正式服务。这样"能不能用"在进入后台之前就已经验证过一次。
package setup

import (
	"context"
	crand "crypto/rand"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"blog-server/internal/auth"
	"blog-server/internal/config"
	"blog-server/internal/search"
	"blog-server/internal/store"

	"github.com/jackc/pgx/v5"
	"github.com/joho/godotenv"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

//go:embed setup.html
var page []byte

// Values 是向导表单的全部输入，字段与 .env 一一对应。
type Values struct {
	Port          string `json:"port"`
	PublicBaseURL string `json:"publicBaseUrl"`
	AdminUser     string `json:"adminUser"`
	AdminPassword string `json:"adminPassword"`

	DBHost     string `json:"dbHost"`
	DBPort     string `json:"dbPort"`
	DBName     string `json:"dbName"`
	DBUser     string `json:"dbUser"`
	DBPassword string `json:"dbPassword"`
	// 直接给完整 DSN 时优先用它（云数据库常要求 sslmode=require）
	DatabaseURL string `json:"databaseUrl"`

	S3Endpoint  string `json:"s3Endpoint"`
	S3AccessKey string `json:"s3AccessKey"`
	S3SecretKey string `json:"s3SecretKey"`
	S3Bucket    string `json:"s3Bucket"`
	S3UseSSL    bool   `json:"s3UseSSL"`

	MeiliHost   string `json:"meiliHost"`
	MeiliAPIKey string `json:"meiliApiKey"`
	MeiliIndex  string `json:"meiliIndex"`
}

// Options 决定向导跑在哪个端口、写哪个 .env、要不要带上次失败原因。
type Options struct {
	EnvPath    string // 空 = 当前目录 .env
	HintsPath  string // 装机脚本生成的预填值（可选）
	Port       string // 空 = PORT 环境变量或 8080
	Force      bool   // 即使 .env 齐全也强制进向导（-setup）
	Reason     string // 显示在页面顶部的提示，例如上次启动失败的原因
	SetupPort  string // 覆盖监听端口，调试用
	ListenHost string // 空 = 0.0.0.0
}

// Result 是向导完成后可以直接接着启动的配置。
type Result struct {
	Cfg *config.Config
}

// Run 阻塞直到用户点完"保存并启动"（返回配置）或进程收到退出信号（返回 error）。
func Run(ctx context.Context, opt Options) (*config.Config, error) {
	envPath := opt.EnvPath
	if envPath == "" {
		envPath = ".env"
	}
	if !filepath.IsAbs(envPath) {
		if abs, err := filepath.Abs(envPath); err == nil {
			envPath = abs
		}
	}

	port := opt.Port
	if opt.SetupPort != "" {
		port = opt.SetupPort
	}
	if port == "" {
		port = os.Getenv("PORT")
	}
	if port == "" {
		port = "8080"
	}

	s := &wizard{
		envPath:  envPath,
		hints:    readHints(opt.HintsPath),
		reason:   opt.Reason,
		defaults: defaultsFromEnv(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handlePage)
	mux.HandleFunc("/api/setup/state", s.handleState)
	mux.HandleFunc("/api/setup/test", s.handleTest)
	mux.HandleFunc("/api/setup/apply", s.handleApply)

	srv := &http.Server{Addr: net.JoinHostPort(opt.ListenHost, port), Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	s.done = make(chan *config.Config, 1)

	go func() {
		log.Printf("尚未完成初始化，配置向导已启动: http://<本机IP>:%s/ （写入 %s）", port, envPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("[setup] 监听 %s 失败: %v", port, err)
		}
	}()

	select {
	case cfg := <-s.done:
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = srv.Close()
		return cfg, nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		return nil, ctx.Err()
	}
}

type wizard struct {
	envPath  string
	hints    map[string]string
	reason   string
	defaults map[string]string
	done     chan *config.Config
}

func (s *wizard) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

// handleState 给前端做首屏预填：.env 现值 + 装机脚本 hints + 本机端口探测。
func (s *wizard) handleState(w http.ResponseWriter, r *http.Request) {
	current := map[string]string{}
	if s.envPath != "" {
		if m, err := godotenv.Read(s.envPath); err == nil {
			for k, v := range m {
				current[k] = v
			}
		}
	}
	v := s.valuesFrom(current)
	if v.Port == "" {
		v.Port = "8080"
	}
	if v.DBPort == "" {
		v.DBPort = "5432"
	}
	if v.MeiliIndex == "" {
		v.MeiliIndex = "kb_search"
	}
	if v.S3Bucket == "" {
		v.S3Bucket = "kb-files"
	}
	if v.AdminUser == "" {
		v.AdminUser = "admin"
	}
	if v.PublicBaseURL == "" {
		v.PublicBaseURL = "http://" + guessHost(r) + ":" + v.Port
	}
	if v.DBHost == "" {
		v.DBHost = "127.0.0.1"
	}
	if v.S3Endpoint == "" {
		v.S3Endpoint = "http://127.0.0.1:9000"
	}
	if v.MeiliHost == "" {
		v.MeiliHost = "http://127.0.0.1:7700"
	}

	writeJSON(w, map[string]any{
		"values":    v,
		"envPath":   s.envPath,
		"reason":    s.reason,
		"detected":  detectLocal(),
		"dirs":      s.hints["_DIRS"],
		"hasHints":  len(s.hints) > 0,
		"setupPort": "",
	})
}

func (s *wizard) handleTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		Kind   string `json:"kind"`
		Values Values `json:"values"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	var err error
	switch in.Kind {
	case "postgres":
		err = testPostgres(ctx, in.Values)
	case "minio":
		err = testMinio(ctx, in.Values)
	case "meili":
		err = testMeili(ctx, in.Values)
	default:
		writeJSON(w, map[string]any{"ok": false, "message": "未知的检查项: " + in.Kind})
		return
	}
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": friendly(err)})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "message": "连接成功"})
}

// handleApply 真正落地：再测一遍 → 写 .env → 迁移 → 建桶 → 建索引 → 建管理员 → 交还监听。
func (s *wizard) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var v Values
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "请求体不是合法 JSON"})
		return
	}
	v = normalize(v)
	if msg := validate(v); msg != "" {
		writeJSON(w, map[string]any{"ok": false, "message": msg})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	if err := testPostgres(ctx, v); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "数据库: " + friendly(err)})
		return
	}
	if err := testMinio(ctx, v); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "对象存储: " + friendly(err)})
		return
	}
	if err := testMeili(ctx, v); err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": "搜索服务: " + friendly(err)})
		return
	}

	cfg, err := s.persist(ctx, v)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	writeJSON(w, map[string]any{"ok": true, "message": "初始化完成，正在启动服务…"})
	go func() { s.done <- cfg }()
}

// persist 写 .env 并做一次真正的初始化动作，全部成功才返回配置。
func (s *wizard) persist(ctx context.Context, v Values) (*config.Config, error) {
	secret := os.Getenv("JWT_SECRET")
	if existing, err := godotenv.Read(s.envPath); err == nil && existing["JWT_SECRET"] != "" {
		secret = existing["JWT_SECRET"] // 保留旧密钥，别让已登录的人全部掉线
	}
	if secret == "" {
		secret = randomHex(32)
	}

	lines := []string{
		"# 由初始化向导生成（" + time.Now().Format("2006-01-02 15:04:05") + "），改完重启服务生效",
		"APP_ENV=prod",
		"PORT=" + v.Port,
		"PUBLIC_BASE_URL=" + quote(v.PublicBaseURL),
		"DATABASE_URL=" + quote(v.dsn()),
		"",
		"MEILI_HOST=" + quote(v.MeiliHost),
		"MEILI_API_KEY=" + quote(v.MeiliAPIKey),
		"MEILI_INDEX=" + quote(v.MeiliIndex),
		"",
		"S3_ENDPOINT=" + quote(v.S3Endpoint),
		"S3_ACCESS_KEY=" + quote(v.S3AccessKey),
		"S3_SECRET_KEY=" + quote(v.S3SecretKey),
		"S3_BUCKET=" + quote(v.S3Bucket),
		"S3_USE_SSL=" + strconv.FormatBool(v.S3UseSSL),
		"",
		"JWT_SECRET=" + secret,
		"ADMIN_USER=" + quote(v.AdminUser),
		"ADMIN_PASSWORD=" + quote(v.AdminPassword),
		"",
	}
	if err := os.MkdirAll(filepath.Dir(s.envPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败: %w", err)
	}
	if err := os.WriteFile(s.envPath, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		return nil, fmt.Errorf("写入 %s 失败: %w", s.envPath, err)
	}

	// 让后续的 config.Load 拿到刚写进去的值
	for _, kv := range [][2]string{
		{"APP_ENV", "prod"}, {"PORT", v.Port}, {"PUBLIC_BASE_URL", v.PublicBaseURL},
		{"DATABASE_URL", v.dsn()}, {"MEILI_HOST", v.MeiliHost}, {"MEILI_API_KEY", v.MeiliAPIKey},
		{"MEILI_INDEX", v.MeiliIndex}, {"S3_ENDPOINT", v.S3Endpoint}, {"S3_ACCESS_KEY", v.S3AccessKey},
		{"S3_SECRET_KEY", v.S3SecretKey}, {"S3_BUCKET", v.S3Bucket},
		{"S3_USE_SSL", strconv.FormatBool(v.S3UseSSL)},
		{"JWT_SECRET", secret}, {"ADMIN_USER", v.AdminUser}, {"ADMIN_PASSWORD", v.AdminPassword},
	} {
		_ = os.Setenv(kv[0], kv[1])
	}
	cfg, err := config.LoadFrom(s.envPath)
	if err != nil {
		return nil, fmt.Errorf("配置写好后仍无法加载: %w", err)
	}

	// 库不存在就先建库（用 postgres 库作为跳板，只有直填 DSN 时才跳过）
	if err := ensureDatabase(ctx, v); err != nil {
		return nil, fmt.Errorf("建库失败: %v", err)
	}
	if err := store.Migrate(cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("建表失败（数据库能连上但没权限建表？）: %v", err)
	}
	pool, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("迁移后连不上数据库: %v", err)
	}
	defer pool.Close()

	if _, err := store.NewS3(cfg); err != nil {
		return nil, fmt.Errorf("对象存储初始化失败: %v", err)
	}
	if err := search.New(cfg).Ensure(ctx); err != nil {
		return nil, fmt.Errorf("搜索索引初始化失败: %v", err)
	}
	mgr, err := auth.New(pool, cfg)
	if err != nil {
		return nil, fmt.Errorf("初始化认证失败: %v", err)
	}
	if err := mgr.Bootstrap(ctx); err != nil {
		return nil, fmt.Errorf("创建管理员失败: %v", err)
	}
	return cfg, nil
}

// valuesFrom 合并三层来源：装机脚本 hints > .env 现值 > 空。
func (s *wizard) valuesFrom(current map[string]string) Values {
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := s.hints[k]; v != "" {
				return v
			}
			if v := current[k]; v != "" {
				return v
			}
		}
		return ""
	}
	v := Values{
		Port:          get("PORT"),
		PublicBaseURL: get("PUBLIC_BASE_URL"),
		AdminUser:     get("ADMIN_USER"),
		AdminPassword: get("ADMIN_PASSWORD"),

		DatabaseURL: get("DATABASE_URL"),
		DBHost:      get("PGHOST"),
		DBPort:      get("PGPORT"),
		DBName:      get("PGDATABASE"),
		DBUser:      get("PGUSER"),
		DBPassword:  get("PGPASSWORD"),

		S3Endpoint:  get("S3_ENDPOINT"),
		S3AccessKey: get("S3_ACCESS_KEY"),
		S3SecretKey: get("S3_SECRET_KEY"),
		S3Bucket:    get("S3_BUCKET"),
		MeiliHost:   get("MEILI_HOST"),
		MeiliAPIKey: get("MEILI_API_KEY"),
		MeiliIndex:  get("MEILI_INDEX"),
	}
	if v.DatabaseURL != "" {
		if u, err := url.Parse(v.DatabaseURL); err == nil {
			if v.DBHost == "" {
				v.DBHost = u.Hostname()
			}
			if v.DBPort == "" {
				v.DBPort = u.Port()
			}
			if v.DBName == "" {
				v.DBName = strings.TrimPrefix(u.Path, "/")
			}
			if v.DBUser == "" && u.User != nil {
				v.DBUser = u.User.Username()
				if p, ok := u.User.Password(); ok && v.DBPassword == "" {
					v.DBPassword = p
				}
			}
		}
	}
	return v
}

func defaultsFromEnv() map[string]string { return map[string]string{} }

// readHints 读装机脚本留下的预填文件（KEY=VALUE，允许注释与空行）。
func readHints(path string) map[string]string {
	out := map[string]string{}
	if path == "" {
		path = os.Getenv("INSTALL_HINTS")
	}
	if path == "" {
		return out
	}
	m, err := godotenv.Read(path)
	if err != nil {
		log.Printf("[setup] 读取预填文件 %s 失败（忽略）: %v", path, err)
		return out
	}
	for k, v := range m {
		out[k] = v
	}
	if dirs := out["_DIRS"]; dirs != "" {
		out["_DIRS"] = dirs
	}
	return out
}

func (v Values) dsn() string {
	if strings.TrimSpace(v.DatabaseURL) != "" {
		return strings.TrimSpace(v.DatabaseURL)
	}
	return fmt.Sprintf("postgres://%s:%s@%s/%s?sslmode=disable",
		url.QueryEscape(v.DBUser), url.QueryEscape(v.DBPassword), net.JoinHostPort(v.DBHost, v.DBPort), v.DBName)
}

func normalize(v Values) Values {
	v.Port = strings.TrimSpace(v.Port)
	v.PublicBaseURL = strings.TrimSpace(v.PublicBaseURL)
	v.AdminUser = strings.TrimSpace(v.AdminUser)
	v.AdminPassword = strings.TrimSpace(v.AdminPassword)
	v.DatabaseURL = strings.TrimSpace(v.DatabaseURL)
	v.DBHost = strings.TrimSpace(v.DBHost)
	v.DBPort = strings.TrimSpace(v.DBPort)
	v.DBName = strings.TrimSpace(v.DBName)
	v.DBUser = strings.TrimSpace(v.DBUser)
	v.DBPassword = strings.TrimSpace(v.DBPassword)
	v.S3Endpoint = strings.TrimSpace(v.S3Endpoint)
	v.S3AccessKey = strings.TrimSpace(v.S3AccessKey)
	v.S3SecretKey = strings.TrimSpace(v.S3SecretKey)
	v.S3Bucket = strings.TrimSpace(v.S3Bucket)
	v.MeiliHost = strings.TrimSpace(v.MeiliHost)
	v.MeiliAPIKey = strings.TrimSpace(v.MeiliAPIKey)
	v.MeiliIndex = strings.TrimSpace(v.MeiliIndex)
	if v.DBPort == "" {
		v.DBPort = "5432"
	}
	if v.MeiliIndex == "" {
		v.MeiliIndex = "kb_search"
	}
	if v.S3Bucket == "" {
		v.S3Bucket = "kb-files"
	}
	if v.AdminUser == "" {
		v.AdminUser = "admin"
	}
	if v.PublicBaseURL == "" && v.Port != "" {
		v.PublicBaseURL = "http://127.0.0.1:" + v.Port
	}
	if v.S3Endpoint != "" && !strings.Contains(v.S3Endpoint, "://") {
		v.S3Endpoint = "http://" + v.S3Endpoint
	}
	if v.MeiliHost != "" && !strings.Contains(v.MeiliHost, "://") {
		v.MeiliHost = "http://" + v.MeiliHost
	}
	return v
}

var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func validate(v Values) string {
	if v.Port == "" {
		return "应用端口不能为空"
	}
	if p, err := strconv.Atoi(v.Port); err != nil || p < 1 || p > 65535 {
		return "应用端口不是合法端口: " + v.Port
	}
	if v.AdminUser == "" || v.AdminPassword == "" {
		return "管理员账号和口令都要填"
	}
	if len(v.AdminPassword) < 6 {
		return "管理员口令至少 6 位"
	}
	if strings.TrimSpace(v.DatabaseURL) == "" {
		if v.DBHost == "" || v.DBName == "" || v.DBUser == "" {
			return "数据库地址、库名、用户名都要填"
		}
		if p, err := strconv.Atoi(v.DBPort); err != nil || p < 1 || p > 65535 {
			return "数据库端口不是合法端口: " + v.DBPort
		}
	}
	if v.S3Endpoint == "" || v.S3AccessKey == "" || v.S3SecretKey == "" {
		return "对象存储 Endpoint / Access Key / Secret Key 都要填"
	}
	if !bucketRe.MatchString(v.S3Bucket) {
		return "桶名只能用小写字母、数字、点和短横线（3-63 位，首尾不能是符号）"
	}
	if v.MeiliHost == "" {
		return "搜索服务地址不能填"
	}
	return ""
}

// ---------- 三个连接检查 ----------

func testPostgres(ctx context.Context, v Values) error {
	dsn := v.dsn()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		// 库不存在时给出可操作的下一步，而不是一句冷冰冰的 FATAL
		if strings.Contains(err.Error(), "does not exist") && strings.TrimSpace(v.DatabaseURL) == "" {
			return fmt.Errorf("库 %q 还没建：点保存会自动创建（需要当前用户有 CREATE DATABASE 权限）", v.DBName)
		}
		return err
	}
	defer conn.Close(ctx)
	return conn.Ping(ctx)
}

func testMinio(ctx context.Context, v Values) error {
	u, err := url.Parse(v.S3Endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("Endpoint 要是 http(s)://host:port 的形式")
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(v.S3AccessKey, v.S3SecretKey, ""),
		Secure: v.S3UseSSL || u.Scheme == "https",
		Region: "us-east-1",
	})
	if err != nil {
		return err
	}
	// ListBuckets 一句话同时验证"通不通 + 账号对不对 + 有没有权限"
	if _, err := client.ListBuckets(ctx); err != nil {
		return err
	}
	return nil
}

func testMeili(ctx context.Context, v Values) error {
	base := strings.TrimRight(v.MeiliHost, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("连不上 %s：%v", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/health 返回 %d", resp.StatusCode)
	}

	req2, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/indexes", nil)
	if err != nil {
		return err
	}
	if v.MeiliAPIKey != "" {
		req2.Header.Set("Authorization", "Bearer "+v.MeiliAPIKey)
	}
	resp2, err := (&http.Client{Timeout: 5 * time.Second}).Do(req2)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusUnauthorized || resp2.StatusCode == http.StatusForbidden {
		return errors.New("API Key 不对（服务活着但鉴权失败）")
	}
	if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
		return fmt.Errorf("/indexes 返回 %d", resp2.StatusCode)
	}
	return nil
}

// ensureDatabase 在保存时兜底：目标库不存在就用 postgres 库建一个。
func ensureDatabase(ctx context.Context, v Values) error {
	if strings.TrimSpace(v.DatabaseURL) != "" {
		return nil
	}
	dsn := fmt.Sprintf("postgres://%s:%s@%s/postgres?sslmode=disable",
		url.QueryEscape(v.DBUser), url.QueryEscape(v.DBPassword), net.JoinHostPort(v.DBHost, v.DBPort))
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", v.DBName).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	_, err = conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{v.DBName}.Sanitize())
	return err
}

// ---------- 小工具 ----------

func detectLocal() map[string]bool {
	type probe struct{ key, addr string }
	out := map[string]bool{}
	for _, p := range []probe{
		{"postgres", "127.0.0.1:5432"},
		{"meili", "127.0.0.1:7700"},
		{"minio", "127.0.0.1:9000"},
	} {
		conn, err := net.DialTimeout("tcp", p.addr, 700*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			out[p.key] = true
		} else {
			out[p.key] = false
		}
	}
	return out
}

func guessHost(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-Host"); h != "" {
		return strings.Split(h, ":")[0]
	}
	if host, _, err := net.SplitHostPort(r.Host); err == nil {
		if host == "" || host == "0.0.0.0" {
			return "127.0.0.1"
		}
		return host
	}
	if r.Host != "" {
		return r.Host
	}
	return "127.0.0.1"
}

// friendly 把常见驱动报错翻成能直接照做的中文。
func friendly(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return "端口没人监听：服务没启动，或者端口填错了（" + msg + "）"
	case strings.Contains(msg, "password authentication failed"):
		return "口令不对（" + msg + "）"
	case strings.Contains(msg, "no such host"):
		return "主机名解析不了（" + msg + "）"
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "context deadline exceeded"):
		return "连接超时：检查 IP/端口和防火墙（" + msg + "）"
	case strings.Contains(msg, "Access Denied"), strings.Contains(msg, "InvalidAccessKeyId"):
		return "Access Key / Secret Key 不对，或没权限（" + msg + "）"
	case strings.Contains(msg, "certificate"):
		return "证书有问题；自签名的本地 MinIO 请把 Endpoint 用 http（" + msg + "）"
	}
	return msg
}

func quote(s string) string {
	if s == "" {
		return ""
	}
	if strings.ContainsAny(s, " \t#\"'") {
		return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
	}
	return s
}

func randomHex(n int) string {
	const hexdigits = "0123456789abcdef"
	b := make([]byte, n)
	if _, err := crand.Read(b); err != nil {
		// 极端情况下的兜底：拿时间做个可用的伪随机，别因为熵源问题起不来
		seed := time.Now().UnixNano()
		for i := range b {
			b[i] = byte(seed >> uint(i%8*8))
		}
	}
	out := make([]byte, n)
	for i, v := range b {
		out[i] = hexdigits[int(v)%16]
	}
	return string(out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
