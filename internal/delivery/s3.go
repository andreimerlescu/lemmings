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

// S3Target uploads rendered reports to Amazon S3. By default (SEC-10),
// screenshots (both inline HTML data URIs and standalone .artifacts/ objects)
// are omitted unless explicitly enabled via SetIncludeScreenshots(true).
type S3Target struct {
	bucket             string
	prefix             string
	region             string
	includeScreenshots bool
	onDelivered        DeliveredFunc
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

// SetIncludeScreenshots controls whether unscrubbed error screenshots (inline
// HTML images and .artifacts/ objects) are uploaded to S3 (false by default under SEC-10).
func (t *S3Target) SetIncludeScreenshots(include bool) { t.includeScreenshots = include }

// Name returns the target URI description.
func (t *S3Target) Name() string {
	if t.prefix != "" {
		return fmt.Sprintf("s3://%s/%s", t.bucket, t.prefix)
	}
	return fmt.Sprintf("s3://%s", t.bucket)
}

type s3Upload struct {
	label       string
	key         string
	contentType string
	body        []byte
	isArtifact  bool
}

func (t *S3Target) buildUploads(r report.Rendered) ([]s3Upload, error) {
	if err := validateSafeComponent(r.Filename); err != nil {
		return nil, fmt.Errorf("s3 target: invalid filename %q: %w", r.Filename, err)
	}
	domain := extractDomain(r.Domain, r.Filename)
	if err := validateSafeComponent(domain); err != nil {
		return nil, fmt.Errorf("s3 target: invalid domain %q: %w", domain, err)
	}
	for _, a := range r.Artifacts {
		if err := validateSafeComponent(a.Filename); err != nil {
			return nil, fmt.Errorf("s3 target: invalid artifact name %q: %w", a.Filename, err)
		}
	}

	mdBody := r.Markdown
	htmlBody := r.HTML
	jsonBody := r.JSON
	if !t.includeScreenshots {
		if r.RemoteMarkdown != "" {
			mdBody = r.RemoteMarkdown
		}
		if r.RemoteHTML != "" {
			htmlBody = r.RemoteHTML
		}
		if r.RemoteJSON != nil {
			jsonBody = r.RemoteJSON
		}
	}

	var uploads []s3Upload
	for _, f := range []struct {
		ext, label, contentType string
		body                    []byte
	}{
		{".md", "md", "text/markdown; charset=utf-8", []byte(mdBody)},
		{".html", "html", "text/html; charset=utf-8", []byte(htmlBody)},
		{".json", "json", "application/json", jsonBody},
	} {
		if f.body == nil {
			continue
		}
		uploads = append(uploads, s3Upload{
			label:       f.label,
			key:         t.objectKey(r.Filename + f.ext),
			contentType: f.contentType,
			body:        f.body,
		})
	}

	if t.includeScreenshots {
		for _, a := range r.Artifacts {
			uploads = append(uploads, s3Upload{
				label:       a.Filename,
				key:         t.objectKey(r.Filename + ".artifacts/" + a.Filename),
				contentType: a.MIMEType,
				body:        a.Data,
				isArtifact:  true,
			})
		}
	}
	return uploads, nil
}

// Deliver uploads .md, .html, .json (and .artifacts/ screenshots only when
// SetIncludeScreenshots(true) is set) to S3.
func (t *S3Target) Deliver(ctx context.Context, r report.Rendered) error {
	uploads, err := t.buildUploads(r)
	if err != nil {
		return err
	}

	cfg, err := t.loadAWSConfig(ctx)
	if err != nil {
		return fmt.Errorf("s3 target: load aws config: %w", err)
	}
	if cfg.Region == "" {
		return errors.New("s3 target: no AWS region configured; set AWS_REGION or AWS_DEFAULT_REGION, or configure a default region in ~/.aws/config")
	}

	client := s3.NewFromConfig(cfg)
	uploadedArtifacts := false
	for _, u := range uploads {
		if err := t.upload(ctx, client, u.key, u.contentType, u.body); err != nil {
			if u.isArtifact {
				return fmt.Errorf("s3 target: upload artifact %s: %w", u.label, err)
			}
			return fmt.Errorf("s3 target: upload %s: %w", u.label, err)
		}
		if u.isArtifact {
			uploadedArtifacts = true
		} else if t.onDelivered != nil {
			t.onDelivered(u.label, fmt.Sprintf("s3://%s/%s", t.bucket, u.key))
		}
	}
	if uploadedArtifacts && t.onDelivered != nil {
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
