package config

import (
	"fmt"
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

// Config 汇总所有运行期配置。字段名与 .env 的变量一一对应，
// 以后新增配置只改这里 + .env.example，不要在业务代码里直接读 os.Getenv。
type Config struct {
	AppEnv        string
	Port          string
	PublicBaseURL string

	DatabaseURL string

	MeiliHost   string
	MeiliAPIKey string
	MeiliIndex  string

	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool

	JWTSecret string

	// 首次启动时用来创建管理员账号，之后改这里不会覆盖已有口令。
	AdminUser     string
	AdminPassword string
}

// Load 读取 .env（不存在不报错，靠真实环境变量兜底），并做最小校验。
func Load() (*Config, error) {
	return LoadFrom("")
}

// LoadFrom 同上，但指定 .env 路径；envPath 为空时按 godotenv 默认规则找 .env。
func LoadFrom(envPath string) (*Config, error) {
	if envPath == "" {
		_ = godotenv.Load()
	} else {
		_ = godotenv.Load(envPath)
	}

	cfg := &Config{
		AppEnv:        getenv("APP_ENV", "dev"),
		Port:          getenv("PORT", "8080"),
		PublicBaseURL: getenv("PUBLIC_BASE_URL", "http://127.0.0.1:8080"),

		DatabaseURL: os.Getenv("DATABASE_URL"),

		MeiliHost:   getenv("MEILI_HOST", "http://127.0.0.1:7700"),
		MeiliAPIKey: os.Getenv("MEILI_API_KEY"),
		MeiliIndex:  getenv("MEILI_INDEX", "kb_search"),

		S3Endpoint:  os.Getenv("S3_ENDPOINT"),
		S3AccessKey: os.Getenv("S3_ACCESS_KEY"),
		S3SecretKey: os.Getenv("S3_SECRET_KEY"),
		S3Bucket:    getenv("S3_BUCKET", "kb-files"),
		S3UseSSL:    getenvBool("S3_USE_SSL", false),

		JWTSecret: os.Getenv("JWT_SECRET"),

		AdminUser:     getenv("ADMIN_USER", "admin"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
	}

	// 数据库是硬依赖，缺了直接失败，避免跑到一半才炸。
	if cfg.DatabaseURL == "" {
		return nil, fmt.Errorf("DATABASE_URL 未配置")
	}
	if _, err := strconv.Atoi(cfg.Port); err != nil {
		return nil, fmt.Errorf("PORT 不是合法端口: %q", cfg.Port)
	}
	return cfg, nil
}

// Missing 列出还缺哪些必填项。空表示配置完整，可以进初始化向导之外的正常流程。
func (c *Config) Missing() []string {
	var miss []string
	if c.DatabaseURL == "" {
		miss = append(miss, "DATABASE_URL")
	}
	if c.S3Endpoint == "" {
		miss = append(miss, "S3_ENDPOINT")
	}
	if c.S3AccessKey == "" || c.S3SecretKey == "" {
		miss = append(miss, "S3_ACCESS_KEY/S3_SECRET_KEY")
	}
	if c.JWTSecret == "" {
		miss = append(miss, "JWT_SECRET")
	}
	if c.AdminPassword == "" {
		miss = append(miss, "ADMIN_PASSWORD")
	}
	return miss
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
