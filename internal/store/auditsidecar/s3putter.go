package auditsidecar

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
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
// `Endpoint` to `https://<project_ref>.supabase.co/storage/v1/s3` and
// provide the project's S3 access keys via the standard AWS env vars
// (Supabase's S3 API keys map onto AWS_ACCESS_KEY_ID /
// AWS_SECRET_ACCESS_KEY). `UsePathStyle` MUST be true for Supabase.
//
// For real AWS S3: leave `Endpoint` empty and `UsePathStyle` false; the
// SDK resolves the regional endpoint from `Region` and authenticates
// via the ambient IAM role (instance profile, ECS task role, env vars).
type PutterConfig struct {
	Region       string
	Endpoint     string // Empty → default AWS regional endpoint.
	UsePathStyle bool   // Required for Supabase; ignored by AWS S3.
}

// NewS3Putter loads the default AWS SDK configuration (respects
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, IAM role
// credentials, etc.) and constructs a putter pointed at cfg.Endpoint
// (or the default AWS S3 endpoint when empty).
func NewS3Putter(ctx context.Context, cfg PutterConfig) (*S3Putter, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("auditsidecar: loading AWS config: %w", err)
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
