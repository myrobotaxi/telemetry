package auditsidecar

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Putter wraps aws-sdk-go-v2/service/s3.Client and implements
// ObjectPutter. The AWS SDK speaks the S3 wire protocol against any
// S3-compatible endpoint, so this single implementation covers both
// real AWS S3 and Supabase Storage's S3-compatible API.
type S3Putter struct {
	client *s3.Client
}

// PutterConfig controls which S3-compatible endpoint the putter targets.
//
// For Supabase Storage (the default infra for this project): set
// `Endpoint` to `https://<project_ref>.supabase.co/storage/v1/s3`,
// `UsePathStyle = true`, and pass the Supabase S3 access key in
// `AccessKeyID` / `SecretAccessKey`. **No AWS account is required** —
// the credentials are issued by Supabase (Storage → S3 connection) and
// the endpoint points at Supabase. The AWS SDK is just the client
// library; it speaks the S3 wire protocol against any S3-compatible
// server.
//
// For real AWS S3 (not used by this project today): leave
// `Endpoint` and credentials empty; the SDK falls back to the ambient
// AWS credential chain (instance profile, ECS task role, AWS_* env
// vars) and resolves the regional endpoint from `Region`.
type PutterConfig struct {
	Region          string
	Endpoint        string // Empty → default AWS regional endpoint.
	UsePathStyle    bool   // Required for Supabase; ignored by AWS S3.
	AccessKeyID     string // Empty → fall back to ambient credential chain.
	SecretAccessKey string
}

// NewS3Putter constructs a putter pointed at cfg.Endpoint. When
// AccessKeyID is non-empty the putter uses static credentials; otherwise
// it falls back to the AWS SDK's default credential chain so an
// AWS-deployed configuration still works.
func NewS3Putter(ctx context.Context, cfg PutterConfig) (*S3Putter, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	loadOpts := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	if cfg.AccessKeyID != "" {
		loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("auditsidecar: loading S3 client config: %w", err)
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.UsePathStyle {
			o.UsePathStyle = true
		}
	})
	return &S3Putter{client: client}, nil
}

// PutObject writes body to <bucket>/<key>. The service principal is
// expected to hold only the bucket-write capability:
//   - On Supabase: an S3 access key scoped to the audit-sidecar bucket
//     via Supabase Storage policies. Append-only is enforced by RLS
//     denying DELETE for the service principal.
//   - On AWS S3: an IAM role granting s3:PutObject only, with Object
//     Lock providing at-rest immutability.
//
// The two backends differ in tamper-resistance posture; see
// docs/operations/backup-retention.md §2.1 for v1 trade-offs and the
// v2 hardening note.
func (p *S3Putter) PutObject(ctx context.Context, bucket, key string, body []byte) error {
	_, err := p.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("auditsidecar: s3 PutObject(%s/%s): %w", bucket, key, err)
	}
	return nil
}
