package delivery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/andreimerlescu/lemmings/v2/internal/report"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Target uploads rendered reports and standalone screenshot assets to Amazon S3.
type S3Target struct {
	bucket      string
	prefix      string
	region      string
	onDelivered DeliveredFunc
}

// NewS3Target parses an s3://bucket[/prefix] URI into an S3Target.
func NewS3Target(uri string, onDelivered DeliveredFunc) (*S3Target, error) {
	p := strings.TrimPrefix(uri, "s3://")
	if p == "" {
		return nil, fmt.Errorf("s3 target: bucket name must not be empty in %q", uri)
	}
	parts := strings.SplitN(p, "/", 2)
	bucket := parts[0]
	prefix := ""
	if len(parts) == 2 {
		prefix = strings.TrimSuffix(parts[1], "/")
	}
	if bucket == "" {
		return nil, fmt.Errorf("s3 target: bucket name is empty in %q", uri)
	}
	region := os.Getenv("AWS_DEFAULT_REGION")
	if region == "" {
		region = os.Getenv("AWS_REGION")
	}
	return &S3Target{
		bucket:      bucket,
		prefix:      prefix,
		region:      region,
		onDelivered: onDelivered,
	}, nil
}

// Name returns the target URI description.
func (t *S3Target) Name() string {
	if t.prefix != "" {
		return fmt.Sprintf("s3://%s/%s", t.bucket, t.prefix)
	}
	return fmt.Sprintf("s3://%s", t.bucket)
}

// Deliver uploads .md, .html, .json, and any .artifacts/ screenshots to S3.
func (t *S3Target) Deliver(ctx context.Context, r report.Rendered) error {
	cfg, err := t.loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("s3 target: load aws config: %w", err)
	}
	if cfg.Region == "" {
		return errors.New("s3 target: no AWS region configured; set AWS_REGION or AWS_DEFAULT_REGION, or configure a default region in ~/.aws/config")
	}

	client := s3.NewFromConfig(cfg)
	for _, f := range []struct {
		ext, label, contentType string
		body                    []byte
	}{
		{".md", "md", "text/markdown; charset=utf-8", []byte(r.Markdown)},
		{".html", "html", "text/html; charset=utf-8", []byte(r.HTML)},
		{".json", "json", "application/json", r.JSON},
	} {
		if f.body == nil {
			continue
		}
		key := t.objectKey(r.Filename + f.ext)
		if err := t.upload(ctx, client, key, f.contentType, f.body); err != nil {
			return fmt.Errorf("s3 target: upload %s: %w", f.label, err)
		}
		if t.onDelivered != nil {
			t.onDelivered(f.label, fmt.Sprintf("s3://%s/%s", t.bucket, key))
		}
	}

	for _, a := range r.Artifacts {
		key := t.objectKey(r.Filename + ".artifacts/" + a.Filename)
		if err := t.upload(ctx, client, key, a.MIMEType, a.Data); err != nil {
			return fmt.Errorf("s3 target: upload artifact %s: %w", a.Filename, err)
		}
	}
	if len(r.Artifacts) > 0 && t.onDelivered != nil {
		t.onDelivered("assets", fmt.Sprintf("s3://%s/%s", t.bucket, t.objectKey(r.Filename+".artifacts/")))
	}
	return nil
}

func (t *S3Target) upload(ctx context.Context, client *s3.Client, key, contentType string, body []byte) error {
	_, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(t.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(contentType),
	})
	return err
}

func (t *S3Target) objectKey(filename string) string {
	if t.prefix == "" {
		return filename
	}
	return t.prefix + "/" + filename
}

func (t *S3Target) loadAWSConfig(ctx context.Context) (aws.Config, error) {
	var opts []func(*config.LoadOptions) error
	if t.region != "" {
		opts = append(opts, config.WithRegion(t.region))
	}
	return config.LoadDefaultConfig(ctx, opts...)
}
