package store

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"blog-server/internal/config"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// NewS3 创建 MinIO 客户端。注意 .env 里 S3_ENDPOINT 带 http:// 前缀，
// 而 minio-go 要求 host:port 形式，所以这里要拆一次。
func NewS3(cfg *config.Config) (*minio.Client, error) {
	u, err := url.Parse(cfg.S3Endpoint)
	if err != nil {
		return nil, fmt.Errorf("解析 S3_ENDPOINT 失败: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3_ENDPOINT 缺少 host:port: %q", cfg.S3Endpoint)
	}

	secure := cfg.S3UseSSL || u.Scheme == "https"
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: secure,
		// 本地 MinIO 不校验 Region，但 SDK 的签名流程需要它非空
		Region: "us-east-1",
	})
	if err != nil {
		return nil, fmt.Errorf("创建 MinIO 客户端失败: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	exists, err := client.BucketExists(ctx, cfg.S3Bucket)
	if err != nil {
		return nil, fmt.Errorf("检查桶 %q 失败: %w", cfg.S3Bucket, err)
	}
	if !exists {
		// 开发期自动建桶；生产建议手工建，避免应用带写权限之外的副作用
		if err := client.MakeBucket(ctx, cfg.S3Bucket, minio.MakeBucketOptions{Region: "us-east-1"}); err != nil {
			return nil, fmt.Errorf("创建桶 %q 失败: %w", cfg.S3Bucket, err)
		}
	}
	return client, nil
}
