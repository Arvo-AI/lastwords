package agent

import (
	"context"
	"os"
	"path"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Arvo-AI/lastwords/internal/store"
)

// uploader copies committed captures to S3-compatible storage (AWS S3, GCS
// interoperability, MinIO). Credentials come from the environment, a mounted
// AWS credentials file, or the node / pod IAM role (IRSA, EKS Pod Identity).
type uploader struct {
	cl     *minio.Client
	bucket string
	prefix string
}

func newUploader(cfg Config) (*uploader, error) {
	endpoint := cfg.S3.Endpoint
	if endpoint == "" {
		endpoint = "s3.amazonaws.com"
	}
	creds := credentials.NewChainCredentials([]credentials.Provider{
		&credentials.EnvAWS{},
		&credentials.FileAWSCredentials{},
		&credentials.IAM{},
	})
	cl, err := minio.New(endpoint, &minio.Options{Creds: creds, Secure: !cfg.S3.Insecure, Region: cfg.S3.Region})
	if err != nil {
		return nil, err
	}
	prefix := cfg.S3.Prefix
	if cfg.Cluster != "" {
		prefix = path.Join(prefix, cfg.Cluster)
	}
	return &uploader{cl: cl, bucket: cfg.S3.Bucket, prefix: path.Join(prefix, cfg.NodeName)}, nil
}

// Upload puts every file of the capture under <prefix>/<cluster>/<node>/<id>/
// and returns the s3:// location.
func (u *uploader) Upload(ctx context.Context, dir string, c *store.Capture) (string, error) {
	base := path.Join(u.prefix, c.ID)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		key := path.Join(base, e.Name())
		if _, err := u.cl.FPutObject(ctx, u.bucket, key, filepath.Join(dir, e.Name()), minio.PutObjectOptions{}); err != nil {
			return "", err
		}
	}
	return "s3://" + u.bucket + "/" + base + "/", nil
}
