// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package s3 provides support for state files stored in S3-compatible object
// stores, such as Amazon S3, MinIO, SeaweedFS or Ceph's RADOS Gateway.
package s3

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/ratelimit"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointfiles"
	"gvisor.dev/gvisor/pkg/sentry/state/checkpointimage"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

const (
	// Read tuning parameters. 16 MiB reads, 8 in flight, deliver about
	// 950 MiB/s from a SeaweedFS store answering ranged GETs in about 1 ms,
	// and the pages that a fault waits for after one request; 1 MiB reads
	// deliver half as much.
	manifestFileMaxReadBytes        = 1 << 20 // 1 MiB
	manifestFileMaxReadParallel     = 2
	miscFileMaxReadBytes            = 1 << 20 // 1 MiB
	miscFileMaxReadParallel         = 8
	defaultPagesFileMaxReadBytes    = 16 << 20 // 16 MiB
	defaultPagesFileMaxReadParallel = 8

	// Write tuning parameters. Files are uploaded in parts of a multipart
	// upload; a file smaller than a part is uploaded with one PUT. Writer
	// holds at most maxParallelParts+1 parts in memory.
	manifestFileMaxWriteBytes        = 2 << 20 // 2 MiB
	manifestFileMaxWriteParallel     = 2
	miscFileMaxWriteBytes            = 2 << 20 // 2 MiB
	miscFileMaxWriteParallel         = 4
	miscFilePartBytes                = 8 << 20 // 8 MiB
	miscFileMaxParallelParts         = 2
	pagesFileMaxWriteBytes           = 32 << 20 // 32 MiB
	pagesFileMaxWriteParallel        = 4
	defaultPagesFilePartBytes        = 32 << 20 // 32 MiB
	defaultPagesFileMaxParallelParts = 4

	// minPartBytes and maxPartBytes bound the size of the parts of a
	// multipart upload, other than the last.
	minPartBytes = 5 << 20 // 5 MiB
	maxPartBytes = 5 << 30 // 5 GiB

	// Defaults of FileServerOptions.
	defaultMaxAttempts         = 5
	defaultRequestTimeout      = 30 * time.Second
	defaultMinRequestBandwidth = 1 << 20 // 1 MiB/s

	// defaultRegion is the region requests to a store other than AWS are
	// signed for if none is configured, as most such stores expect.
	defaultRegion = "us-east-1"

	// contentType is the Content-Type of the objects this package creates.
	contentType = "application/octet-stream"
)

// retryMaxBackoff is the maximum delay between attempts of a request. It is a
// variable for tests.
var retryMaxBackoff = 20 * time.Second

// FileServer implements stateipc.AsyncFileServerImpl for checkpoint files in
// an S3 bucket.
type FileServer struct {
	ctx    context.Context
	cancel context.CancelCauseFunc
	client *s3.Client
	opts   FileServerOptions
	retry  retryPolicy
}

// FileServerOptions provides options to NewFileServer.
type FileServerOptions struct {
	// AllowCheckpointReads enables checkpoint reading.
	AllowCheckpointReads bool

	// AllowCheckpointWrites enables checkpoint writing.
	AllowCheckpointWrites bool

	// AllowFSCheckpointReads enables filesystem checkpoint reading.
	AllowFSCheckpointReads bool

	// AllowFSCheckpointWrites enables filesystem checkpoint writing.
	AllowFSCheckpointWrites bool

	// Endpoint is the URL of the store, e.g. "https://s3.example.com". If it
	// is empty, the AWS endpoint of Region is used.
	Endpoint string

	// Region is the region that requests are signed for. If it is empty, the
	// AWS SDK's configuration (AWS_REGION, the shared configuration file)
	// provides it, or, with an Endpoint, it is "us-east-1".
	Region string

	// Bucket is the bucket containing checkpoint files.
	Bucket string

	// ObjectPrefix is prepended to each filename to form object keys.
	ObjectPrefix string

	// UsePathStyle addresses the bucket in the URL's path
	// (https://host/bucket/key) rather than in its host name
	// (https://bucket.host/key), as most stores other than AWS require.
	UsePathStyle bool

	// If Credentials is not nil, it provides the credentials that sign
	// requests. Otherwise, the AWS SDK's default chain provides them: the
	// environment, the shared credentials and configuration files (with
	// SharedCredentialsFile and Profile, if set), web identity, and container
	// and instance metadata.
	Credentials           aws.CredentialsProvider
	SharedCredentialsFile string
	Profile               string

	// MaxAttempts is the number of attempts of each request, which are
	// retried on throttling, server errors and connection errors with
	// exponential backoff. If it is 0, it is 5.
	MaxAttempts int

	// RequestTimeout bounds each request, with its retries, plus the time to
	// transfer its bytes at MinRequestBandwidth (bytes per second), so that a
	// stalled store fails a checkpoint or restore rather than hangs it. If
	// they are 0, they are 30 seconds and 1 MiB/s.
	RequestTimeout      time.Duration
	MinRequestBandwidth uint64

	// PagesFileReadBytes and PagesFileReadParallel are the size of reads of
	// the pages file and the number of reads in flight. If they are 0, they
	// are 16 MiB and 8.
	PagesFileReadBytes    uint64
	PagesFileReadParallel int

	// PagesFilePartBytes and PagesFileWriteParallel are the size of the parts
	// in which the pages file is uploaded and the number of parts uploaded at
	// once. If they are 0, they are 32 MiB and 4.
	PagesFilePartBytes     int
	PagesFileWriteParallel int
}

// retryPolicy bounds the requests of a FileServer.
type retryPolicy struct {
	maxAttempts int
	timeout     time.Duration
	bandwidth   uint64
}

// deadline returns the time that a request transferring n bytes may take,
// with its retries.
func (p retryPolicy) deadline(n uint64) time.Duration {
	return p.timeout + time.Duration(float64(n)/float64(p.bandwidth)*float64(time.Second))
}

// NewFileServer returns a new FileServer.
func NewFileServer(ctx context.Context, opts *FileServerOptions) (*FileServer, error) {
	o := *opts
	if len(o.Bucket) == 0 {
		return nil, fmt.Errorf("S3 bucket must be specified")
	}
	if o.MaxAttempts == 0 {
		o.MaxAttempts = defaultMaxAttempts
	}
	if o.RequestTimeout == 0 {
		o.RequestTimeout = defaultRequestTimeout
	}
	if o.MinRequestBandwidth == 0 {
		o.MinRequestBandwidth = defaultMinRequestBandwidth
	}
	if o.PagesFileReadBytes == 0 {
		o.PagesFileReadBytes = defaultPagesFileMaxReadBytes
	}
	if o.PagesFileReadParallel == 0 {
		o.PagesFileReadParallel = defaultPagesFileMaxReadParallel
	}
	if o.PagesFilePartBytes == 0 {
		o.PagesFilePartBytes = defaultPagesFilePartBytes
	}
	if o.PagesFileWriteParallel == 0 {
		o.PagesFileWriteParallel = defaultPagesFileMaxParallelParts
	}
	pageSize := uint64(os.Getpagesize())
	switch {
	case o.MaxAttempts < 0 || o.RequestTimeout < 0 || o.PagesFileReadParallel < 0 || o.PagesFileWriteParallel < 0:
		// Not %+v of o, which would print its credentials.
		return nil, fmt.Errorf("negative S3 options: max attempts %d, request timeout %v, pages file read parallelism %d, write parallelism %d", o.MaxAttempts, o.RequestTimeout, o.PagesFileReadParallel, o.PagesFileWriteParallel)
	case o.PagesFileReadBytes%pageSize != 0:
		return nil, fmt.Errorf("pages file read size %d is not a multiple of the page size %d", o.PagesFileReadBytes, pageSize)
	case o.PagesFilePartBytes < minPartBytes || o.PagesFilePartBytes > maxPartBytes:
		return nil, fmt.Errorf("pages file part size %d is not between %d and %d", o.PagesFilePartBytes, minPartBytes, maxPartBytes)
	}

	if o.SharedCredentialsFile != "" {
		fi, err := os.Stat(o.SharedCredentialsFile)
		if err != nil {
			return nil, fmt.Errorf("credentials file: %w", err)
		}
		if err := CheckFileMode(o.SharedCredentialsFile, fi, true /* secret */); err != nil {
			return nil, err
		}
	}

	maxConns := manifestFileMaxReadParallel + 2*miscFileMaxReadParallel + o.PagesFileReadParallel
	maxConns = max(maxConns, 2*miscFileMaxParallelParts+o.PagesFileWriteParallel)
	cfgOpts := []func(*config.LoadOptions) error{
		config.WithRetryer(func() aws.Retryer {
			return retry.NewStandard(func(so *retry.StandardOptions) {
				so.MaxAttempts = o.MaxAttempts
				so.MaxBackoff = retryMaxBackoff
				// Without the SDK's client-wide quota of retries, which
				// would fail every read of a restore after a burst of
				// errors, however briefly the store failed.
				so.RateLimiter = ratelimit.None
			})
		}),
		config.WithHTTPClient(&http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConnsPerHost: maxConns,
				// Disable HTTP/2 for performance, as the GCS checkpoint
				// gofer does.
				TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
			},
		}),
		// Checksums of whole objects cannot check ranged reads, and stores
		// other than AWS do not all accept the trailing checksums of
		// uploads; checkpoint images carry their own.
		config.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		config.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	}
	if o.Region != "" {
		cfgOpts = append(cfgOpts, config.WithRegion(o.Region))
	}
	if o.Credentials != nil {
		cfgOpts = append(cfgOpts, config.WithCredentialsProvider(o.Credentials))
	}
	if o.SharedCredentialsFile != "" {
		cfgOpts = append(cfgOpts, config.WithSharedCredentialsFiles([]string{o.SharedCredentialsFile}))
	}
	if o.Profile != "" {
		cfgOpts = append(cfgOpts, config.WithSharedConfigProfile(o.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, cfgOpts...)
	if err != nil {
		return nil, fmt.Errorf("failed to load the AWS SDK's configuration: %w", err)
	}
	if cfg.Region == "" && o.Endpoint != "" {
		cfg.Region = defaultRegion
	}
	client := s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
		}
		so.UsePathStyle = o.UsePathStyle
	})

	ctx, cancel := context.WithCancelCause(ctx)
	return &FileServer{
		ctx:    ctx,
		cancel: cancel,
		client: client,
		opts:   o,
		retry: retryPolicy{
			maxAttempts: o.MaxAttempts,
			timeout:     o.RequestTimeout,
			bandwidth:   o.MinRequestBandwidth,
		},
	}, nil
}

// CheckFileMode returns an error unless the file described by fi, named name,
// is owned by root or by the current user, may be written by no one else, and,
// if secret (it holds credentials), may be read by no one else. Users who may
// write an options file choose where checkpoints, which hold the memory of a
// sandbox, are written and read from; users who may read credentials can use
// them. As ssh does with private keys, the checkpoint gofer refuses such files
// rather than warn about them in logs that no one reads.
func CheckFileMode(name string, fi fs.FileInfo, secret bool) error {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: unknown owner", name)
	}
	return checkFileMode(name, fi.Mode().Perm(), st.Uid, uint32(os.Geteuid()), secret)
}

// checkFileMode implements CheckFileMode for a file with permissions perm and
// owner owner, checked for user euid.
func checkFileMode(name string, perm fs.FileMode, owner, euid uint32, secret bool) error {
	switch {
	case owner != 0 && owner != euid:
		return fmt.Errorf("%s is owned by user %d, neither root nor the checkpoint gofer's user %d", name, owner, euid)
	case perm&0o022 != 0:
		return fmt.Errorf("%s may be written by users other than its owner (mode %#o); make it writable by its owner only, e.g. with chmod 600", name, perm)
	case secret && perm&0o044 != 0:
		return fmt.Errorf("%s holds credentials but may be read by users other than its owner (mode %#o); make it readable by its owner only, e.g. with chmod 600", name, perm)
	}
	return nil
}

// Destroy implements stateipc.AsyncFileServerImpl.Destroy.
func (s *FileServer) Destroy() {
	s.cancel(fmt.Errorf("context canceled by s3.FileServer.Destroy"))
}

// OpenRead implements stateipc.AsyncFileServerImpl.OpenRead.
func (s *FileServer) OpenRead(path string) (stateio.AsyncReader, error) {
	// Files other than the pages file are read using stateio.BufReader, which
	// doesn't use MaxRanges > 1.
	// The layers of an image (see checkpointimage.LayerPath) are read as its
	// pages metadata and pages files are.
	name := path
	if _, layerFile, ok := checkpointimage.ParseLayerPath(path); ok && (layerFile == checkpointfiles.PagesMetadataFileName || layerFile == checkpointfiles.PagesFileName) {
		name = layerFile
	}
	var (
		allowed      bool
		maxReadBytes uint64
		maxRanges    = 1
		maxParallel  int
	)
	switch name {
	case checkpointfiles.StateFileName:
		allowed = s.opts.AllowCheckpointReads
		maxReadBytes, maxParallel = miscFileMaxReadBytes, miscFileMaxReadParallel
	case checkpointfiles.FSCheckpointManifestFileName:
		allowed = s.opts.AllowFSCheckpointReads
		maxReadBytes, maxParallel = manifestFileMaxReadBytes, manifestFileMaxReadParallel
	case checkpointfiles.FSCheckpointMultiTarFileName:
		allowed = s.opts.AllowFSCheckpointReads
		maxReadBytes, maxParallel = miscFileMaxReadBytes, miscFileMaxReadParallel
	case checkpointfiles.PagesMetadataFileName:
		allowed = s.opts.AllowCheckpointReads || s.opts.AllowFSCheckpointReads
		maxReadBytes, maxParallel = miscFileMaxReadBytes, miscFileMaxReadParallel
	case checkpointfiles.PagesFileName:
		allowed = s.opts.AllowCheckpointReads || s.opts.AllowFSCheckpointReads
		// Provision one range per page, which is the most that
		// pgalloc.MemoryFile restore can require. Since Reader doesn't (can't)
		// use readv, we aren't subject to UIO_MAXIOV.
		maxReadBytes, maxParallel = s.opts.PagesFileReadBytes, s.opts.PagesFileReadParallel
		maxRanges = int(maxReadBytes / uint64(os.Getpagesize()))
	default:
		log.Warningf("s3.FileServer.OpenRead: unknown path %q", path)
		return nil, fs.ErrPermission
	}
	if !allowed {
		log.Warningf("s3.FileServer.OpenRead: attempted to open %q, which this checkpoint gofer may not read", path)
		return nil, fs.ErrPermission
	}
	obj := s.object(path)
	log.Infof("Opening %s for reading", obj)
	return newReader(s.ctx, s.client, obj, s.retry, maxReadBytes, maxRanges, maxParallel), nil
}

// OpenWrite implements stateipc.AsyncFileServerImpl.OpenWrite.
func (s *FileServer) OpenWrite(path string) (stateio.AsyncWriter, error) {
	// Files other than the pages file are written using stateio.BufWriter,
	// which doesn't use MaxRanges > 1.
	var (
		allowed          bool
		maxWriteBytes    uint64
		maxRanges        = 1
		maxParallel      int
		partBytes        = miscFilePartBytes
		maxParallelParts = miscFileMaxParallelParts
	)
	switch path {
	case checkpointfiles.StateFileName:
		allowed = s.opts.AllowCheckpointWrites
		maxWriteBytes, maxParallel = miscFileMaxWriteBytes, miscFileMaxWriteParallel
	case checkpointfiles.FSCheckpointManifestFileName:
		allowed = s.opts.AllowFSCheckpointWrites
		maxWriteBytes, maxParallel = manifestFileMaxWriteBytes, manifestFileMaxWriteParallel
	case checkpointfiles.FSCheckpointMultiTarFileName:
		allowed = s.opts.AllowFSCheckpointWrites
		maxWriteBytes, maxParallel = miscFileMaxWriteBytes, miscFileMaxWriteParallel
	case checkpointfiles.PagesMetadataFileName:
		allowed = s.opts.AllowCheckpointWrites || s.opts.AllowFSCheckpointWrites
		maxWriteBytes, maxParallel = miscFileMaxWriteBytes, miscFileMaxWriteParallel
	case checkpointfiles.PagesFileName:
		allowed = s.opts.AllowCheckpointWrites || s.opts.AllowFSCheckpointWrites
		// Provision one range per page, which is the most that
		// pgalloc.MemoryFile saving can require. Since Writer doesn't (can't)
		// use writev, we aren't subject to UIO_MAXIOV.
		maxWriteBytes, maxParallel = pagesFileMaxWriteBytes, pagesFileMaxWriteParallel
		maxRanges = int(maxWriteBytes / uint64(os.Getpagesize()))
		partBytes, maxParallelParts = s.opts.PagesFilePartBytes, s.opts.PagesFileWriteParallel
	default:
		log.Warningf("s3.FileServer.OpenWrite: unknown path %q", path)
		return nil, fs.ErrPermission
	}
	if !allowed {
		log.Warningf("s3.FileServer.OpenWrite: attempted to open %q, which this checkpoint gofer may not write", path)
		return nil, fs.ErrPermission
	}
	obj := s.object(path)
	log.Infof("Opening %s for writing", obj)
	return newWriter(s.ctx, s.client, obj, s.retry, maxWriteBytes, maxRanges, maxParallel, partBytes, maxParallelParts), nil
}

// object returns the object holding the file at path.
func (s *FileServer) object(path string) object {
	return object{bucket: s.opts.Bucket, key: s.opts.ObjectPrefix + path}
}

// object identifies an object.
type object struct {
	bucket string
	key    string
}

// String implements fmt.Stringer.String.
func (o object) String() string {
	return fmt.Sprintf("s3://%s/%s", o.bucket, o.key)
}
