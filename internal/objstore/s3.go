package objstore

// S3-compatible backend (spec §4.1: MinIO in dev, any S3 in prod).
// Configuration: objstore.backend: s3, endpoint from the env named by
// objstore.endpoint_env (empty = real AWS), credentials from
// MEMD_S3_ACCESS_KEY / MEMD_S3_SECRET_KEY or the standard AWS chain.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/lazorfuzz/memba/internal/config"
)

// S3 implements Store over an S3-compatible endpoint.
type S3 struct {
	client *s3.Client
	bucket string
}

// NewS3 builds the client and ensures the bucket exists (MinIO dev
// convenience; on AWS the CreateBucket no-ops or fails harmlessly if owned).
func NewS3(ctx context.Context, cfg config.ObjStore) (*S3, error) {
	endpoint := os.Getenv(cfg.EndpointEnv)
	var loadOpts []func(*awsconfig.LoadOptions) error
	if os.Getenv("AWS_REGION") == "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion("us-east-1"))
	}
	if ak, sk := os.Getenv("MEMD_S3_ACCESS_KEY"), os.Getenv("MEMD_S3_SECRET_KEY"); ak != "" && sk != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(ak, sk, "")))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("s3 config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = &endpoint
			o.UsePathStyle = true // MinIO and most S3-compatibles
		}
	})
	st := &S3{client: client, bucket: cfg.Bucket}
	if err := st.ensureBucket(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

func (s *S3) ensureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &s.bucket})
	if err == nil {
		return nil
	}
	_, cerr := s.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &s.bucket})
	if cerr == nil {
		return nil
	}
	var owned *types.BucketAlreadyOwnedByYou
	var exists *types.BucketAlreadyExists
	if errors.As(cerr, &owned) || errors.As(cerr, &exists) {
		return nil
	}
	return fmt.Errorf("s3 bucket %q unavailable: head=%v create=%v", s.bucket, err, cerr)
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: &s.bucket, Key: &key, Body: r,
	})
	return err
}

func (s *S3) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		var apiErr smithy.APIError
		if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "NotFound") {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return out.Body, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &key})
	return err
}
