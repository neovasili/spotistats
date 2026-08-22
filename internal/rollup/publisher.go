package rollup

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// DataPrefix is where snapshots live in the bucket, matching the CloudFront /data/* behaviour.
const DataPrefix = "data/"

// S3API and CloudFrontAPI are the narrow seams the publisher needs, so it is testable with
// fakes and so nothing else in this package touches AWS.
type S3API interface {
	PutObject(context.Context, *s3.PutObjectInput, ...func(*s3.Options)) (*s3.PutObjectOutput, error)
}

type CloudFrontAPI interface {
	CreateInvalidation(context.Context, *cloudfront.CreateInvalidationInput, ...func(*cloudfront.Options)) (*cloudfront.CreateInvalidationOutput, error)
}

// S3Publisher writes snapshots to the site bucket and invalidates the CDN.
type S3Publisher struct {
	s3             S3API
	cdn            CloudFrontAPI
	bucket         string
	distributionID string
	now            func() time.Time
}

// NewS3Publisher returns a publisher. cdn and distributionID may be empty, in which case
// invalidation is skipped and the edge serves the previous snapshot until its TTL expires.
func NewS3Publisher(s3c S3API, cdn CloudFrontAPI, bucket, distributionID string, now func() time.Time) *S3Publisher {
	if now == nil {
		now = time.Now
	}
	return &S3Publisher{s3: s3c, cdn: cdn, bucket: bucket, distributionID: distributionID, now: now}
}

// Publish writes one snapshot.
//
// Cache-Control matches docs/SPECS.md 4.3: five minutes in the browser, a day at the edge. The
// long edge TTL is safe because a successful run invalidates /data/* immediately; the TTL is the
// fallback for when invalidation fails, and serving yesterday's dashboard for a while is a much
// better failure than serving none.
func (p *S3Publisher) Publish(ctx context.Context, name string, body []byte) error {
	_, err := p.s3.PutObject(ctx, &s3.PutObjectInput{
		Bucket:       aws.String(p.bucket),
		Key:          aws.String(DataPrefix + name),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String("application/json; charset=utf-8"),
		CacheControl: aws.String("public, max-age=300, s-maxage=86400"),
	})
	if err != nil {
		return fmt.Errorf("rollup: put s3://%s/%s%s: %w", p.bucket, DataPrefix, name, err)
	}
	return nil
}

func (p *S3Publisher) Invalidate(ctx context.Context, paths []string) error {
	if p.cdn == nil || p.distributionID == "" || len(paths) == 0 {
		return nil
	}
	// A caller reference must be unique per invalidation; the run timestamp serves.
	ref := fmt.Sprintf("spotistats-%d", p.now().UnixNano())
	items := make([]string, len(paths))
	copy(items, paths)

	_, err := p.cdn.CreateInvalidation(ctx, &cloudfront.CreateInvalidationInput{
		DistributionId: aws.String(p.distributionID),
		InvalidationBatch: &cftypes.InvalidationBatch{
			CallerReference: aws.String(ref),
			Paths: &cftypes.Paths{
				Quantity: aws.Int32(int32(len(items))),
				Items:    items,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("rollup: invalidate %v: %w", items, err)
	}
	return nil
}

// DirPublisher writes snapshots to a local directory.
//
// This is what makes the offline frontend loop complete: `spotistats rollup` renders into a
// directory and `spotistats serve -data <dir>` hosts it, so the dashboard renders with no AWS at
// all (docs/SPECS.md 7.4).
type DirPublisher struct{ dir string }

func NewDirPublisher(dir string) *DirPublisher { return &DirPublisher{dir: dir} }

// Dir reports the target directory, for operator-facing messages.
func (p *DirPublisher) Dir() string { return p.dir }

func (p *DirPublisher) Publish(ctx context.Context, name string, body []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(p.dir, 0o755); err != nil {
		return fmt.Errorf("rollup: create %s: %w", p.dir, err)
	}
	path := filepath.Join(p.dir, name)

	// Written via a temp file and renamed: a half-written dashboard.json would make the local
	// dev server serve a JSON parse error, which looks like a frontend bug.
	tmp, err := os.CreateTemp(p.dir, "."+name+"-*")
	if err != nil {
		return fmt.Errorf("rollup: create temp file in %s: %w", p.dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("rollup: write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Invalidate is a no-op: there is no CDN in front of a local directory.
func (p *DirPublisher) Invalidate(context.Context, []string) error { return nil }
