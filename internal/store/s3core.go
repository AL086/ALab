package store

import (
	"fmt"
	"net/url"

	"blog-server/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// NewS3Core 创建低层客户端，用来做分片上传（multipart）。
// minio.Client 没有暴露 multipart 的底层接口，必须用 Core。
func NewS3Core(cfg *config.Config) (*minio.Core, error) {
	u, err := url.Parse(cfg.S3Endpoint)
	if err != nil {
		return nil, fmt.Errorf("解析 S3_ENDPOINT 失败: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3_ENDPOINT 缺少 host:port: %q", cfg.S3Endpoint)
	}

	secure := cfg.S3UseSSL || u.Scheme == "https"
	core, err := minio.NewCore(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: secure,
		Region: "us-east-1",
	})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO Core 客户端失败: %w", err)
	}
	return core, nil
}
