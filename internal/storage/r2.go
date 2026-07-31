package storage

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type R2Storage struct {
	client     *s3.Client
	bucket     string
	publicBase string
}

// NewR2Storage 构造打 Cloudflare R2 的 S3 客户端。
//
// endpoint 传完整地址（而非 account id）是为了让测试能指向 httptest.Server。
func NewR2Storage(endpoint, accessKeyID, secretAccessKey, bucket, publicBaseURL string) *R2Storage {
	client := s3.New(s3.Options{
		// R2 要求 region 固定为 "auto"。
		Region:       "auto",
		BaseEndpoint: aws.String(endpoint),
		// R2 用 path-style（/<bucket>/<key>）。virtual-host style 需要按桶名解析
		// 子域，R2 的 S3 endpoint 不提供。
		UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(
			accessKeyID, secretAccessKey, ""),
		// 只在上游明确要求时才算 checksum。默认的 when_supported 会附加
		// x-amz-checksum-* 头，而 S3 兼容实现对这些头的处理并不一致——打开它
		// 换不到任何东西，却多一类只在真 R2 上才会出现的失败。
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	})
	return &R2Storage{
		client: client,
		bucket: bucket,
		// 去掉末尾斜杠：运维多打一个斜杠是必然会发生的事，而后果是每个 URL 里
		// 出现 // ，有些 CDN 会对此 404。
		publicBase: strings.TrimSuffix(publicBaseURL, "/"),
	}
}

func (s *R2Storage) Put(ctx context.Context, key, contentType string, body []byte) (string, error) {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(body),
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(int64(len(body))),
	})
	if err != nil {
		return "", fmt.Errorf("上传对象 %s: %w", key, err)
	}
	return s.publicBase + "/" + key, nil
}
