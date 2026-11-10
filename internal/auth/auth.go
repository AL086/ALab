// Package auth 负责口令散列与 JWT 签发/校验。
// v1 是单用户（博客所有者），但数据模型按多用户留了 role 字段。
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"blog-server/internal/config"
	"blog-server/internal/response"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"
)

var (
	ErrInvalidCredentials = errors.New("用户名或密码不正确")
	ErrNoToken            = errors.New("缺少凭证")
)

// User 是校验通过后放进 gin.Context 的身份。
type User struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}

type Manager struct {
	pool      *pgxpool.Pool
	secret    []byte
	ttl       time.Duration
	adminUser string
	adminPass string
}

// New 构造 Manager。JWT_SECRET 太短直接拒绝启动，避免用默认值上线。
func New(pool *pgxpool.Pool, cfg *config.Config) (*Manager, error) {
	if len(cfg.JWTSecret) < 16 {
		return nil, fmt.Errorf("JWT_SECRET 太短（至少 16 字符），当前长度 %d", len(cfg.JWTSecret))
	}
	return &Manager{
		pool:      pool,
		secret:    []byte(cfg.JWTSecret),
		ttl:       24 * time.Hour,
		adminUser: cfg.AdminUser,
		adminPass: cfg.AdminPassword,
	}, nil
}

// Bootstrap 在用户表为空时创建初始管理员。
// 口令来自 .env；没配就随机生成并打到日志里（只在本地开发会发生）。
func (m *Manager) Bootstrap(ctx context.Context) error {
	var count int
	if err := m.pool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	username := m.adminUser
	if strings.TrimSpace(username) == "" {
		username = "admin"
	}
	password := m.adminPass
	generated := false
	if password == "" {
		password = randomToken(16)
		generated = true
	}

	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if _, err := m.pool.Exec(ctx,
		`INSERT INTO users (username, password_hash, role) VALUES ($1, $2, 'owner')`,
		username, hash); err != nil {
		return err
	}

	if generated {
		fmt.Printf("首次启动：已创建管理员 %s，随机口令 %s（请立刻记下并改到 .env 的 ADMIN_PASSWORD）\n", username, password)
	} else {
		fmt.Printf("首次启动：已创建管理员 %s（口令取自 ADMIN_PASSWORD）\n", username)
	}
	return nil
}

// Login 校验口令并签发 JWT。失败统一返回 ErrInvalidCredentials，不区分"用户不存在/密码错"。
func (m *Manager) Login(ctx context.Context, username, password string) (string, User, error) {
	var (
		u    User
		hash string
	)
	err := m.pool.QueryRow(ctx,
		`SELECT id, username, role, password_hash FROM users WHERE username = $1`,
		username).Scan(&u.ID, &u.Username, &u.Role, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", User{}, ErrInvalidCredentials
	}
	if err != nil {
		return "", User{}, err
	}
	if !VerifyPassword(hash, password) {
		return "", User{}, ErrInvalidCredentials
	}

	token, err := m.sign(u)
	if err != nil {
		return "", User{}, err
	}
	return token, u, nil
}

func (m *Manager) sign(u User) (string, error) {
	claims := jwt.MapClaims{
		"sub":  u.Username,
		"uid":  u.ID,
		"role": u.Role,
		"iat":  time.Now().Unix(),
		"exp":  time.Now().Add(m.ttl).Unix(),
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
}

// parse 校验签名与过期时间。
func (m *Manager) parse(token string) (User, error) {
	var claims jwt.MapClaims
	_, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("签名算法不支持: %v", t.Header["alg"])
		}
		return m.secret, nil
	})
	if err != nil {
		return User{}, err
	}

	var u User
	if v, ok := claims["uid"].(float64); ok {
		u.ID = int64(v)
	}
	if v, ok := claims["sub"].(string); ok {
		u.Username = v
	}
	if v, ok := claims["role"].(string); ok {
		u.Role = v
	}
	if u.ID == 0 {
		return User{}, ErrNoToken
	}
	return u, nil
}

// tokenFrom 支持两种取法：Authorization: Bearer，以及 ?token=（给浏览器直接下载用）。
func tokenFrom(c *gin.Context) string {
	if h := c.GetHeader("Authorization"); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	return strings.TrimSpace(c.Query("token"))
}

// Middleware 强制鉴权，失败直接 401 并中断。响应体保持和业务接口一致的 envelope。
func (m *Manager) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		u, err := m.parse(tokenFrom(c))
		if err != nil {
			response.Fail(c, 401, response.CodeUnauth, "未登录或凭证已失效")
			c.Abort()
			return
		}
		c.Set("user", u)
		c.Next()
	}
}

// Soft 只尝试解析，不拦截。用于"登录了能看到草稿、没登录只看公开文章"这类接口。
func (m *Manager) Soft(c *gin.Context) (User, bool) {
	if u, ok := c.Get("user"); ok {
		if v, ok := u.(User); ok {
			return v, true
		}
	}
	tk := tokenFrom(c)
	if tk == "" {
		return User{}, false
	}
	u, err := m.parse(tk)
	if err != nil {
		return User{}, false
	}
	c.Set("user", u)
	return u, true
}

// ---------- argon2id ----------

const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64MB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// HashPassword 生成 $argon2id$v=19$m=...,t=...,p=...$salt$hash。
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword 按散列里记录的参数重算，常量时间比较。
func VerifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func randomToken(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
