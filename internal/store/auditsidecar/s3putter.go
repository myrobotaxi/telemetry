package auditsidecar

import (
	"bytes"
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// AWSS3Putter wraps aws-sdk-go-v2/service/s3.Client and implements
// ObjectPutter. This is the production implementation; unit tests inject a
// fake instead.
type AWSS3Putter struct {
	client *s3.Client
}

// NewAWSS3Putter loads the default AWS SDK configuration (respects
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_SESSION_TOKEN, IAM role
// credentials, etc.) and constructs an AWSS3Putter for the given region.
func NewAWSS3Putter(ctx context.Context, region string) (*AWSS3Putter, error) {
	if region == "" {
		region = "us-east-1"
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, fmt.Errorf("auditsidecar: loading AWS config: %w", err)
	}
	return &AWSS3Putter{client: s3.NewFromConfig(cfg)}, nil
}

// PutObject writes body to s3://bucket/key. The IAM service role is expected
// to hold s3:PutObject on the audit sidecar bucket only (see
// deployments/terraform/audit-sidecar/iam.tf). No delete, lifecycle, or
// bucket-policy permissions are granted to this role.
func (p *AWSS3Putter) PutObject(ctx context.Context, bucket, key string, body []byte) error {
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
