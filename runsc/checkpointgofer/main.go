// Copyright 2025 The gVisor Authors.
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

// Binary checkpointgofer implements the checkpoint gofer, which provides
// remote checkpoint file access.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/google/subcommands"
	"golang.org/x/oauth2"
	"gvisor.dev/gvisor/pkg/sentry/state/stateipc"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/urpc"
	"gvisor.dev/gvisor/runsc/checkpointgofer/gcs"
	"gvisor.dev/gvisor/runsc/checkpointgofer/s3"
	"gvisor.dev/gvisor/runsc/cli"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/gvisorbinaries"
)

// GCSOptions holds options that configure a checkpoint gofer to access GCS.
type GCSOptions struct {
	// If Token is non-empty, the checkpoint gofer will use it for
	// authentication.
	//
	// Otherwise, the checkpoint gofer will use application default
	// credentials.
	Token *oauth2.Token `json:"token,omitzero"`

	// Bucket is the GCS bucket containing checkpoint files.
	Bucket string `json:"bucket"`

	// ObjectPrefix is prepended to checkpoint file names to form GCS object
	// names. Note that ObjectPrefix is prepended as a string, not a path, so
	// if ObjectPrefix does not have a trailing "/", no "/" will be inserted
	// between ObjectPrefix and the filename.
	ObjectPrefix string `json:"object_prefix"`

	// ParallelCompositeUpload controls whether parallel composite upload is
	// used for writing the pages file. Valid values are:
	// - "safe": enable parallel composite upload if safe
	// - "disable": disable parallel composite upload unconditionally
	// - "force": enable parallel composite upload unconditionally
	ParallelCompositeUpload string `json:"parallel_composite_upload,omitzero"`
}

// S3Options holds options that configure a checkpoint gofer to access an
// S3-compatible object store.
type S3Options struct {
	// Endpoint is the URL of the store, e.g. "https://s3.example.com". If it
	// is empty, the store is AWS's, at the endpoint of Region.
	Endpoint string `json:"endpoint,omitzero"`

	// Region is the region that requests are signed for. If it is empty, the
	// AWS SDK's configuration provides it (e.g. AWS_REGION), or, with an
	// Endpoint, it is "us-east-1".
	Region string `json:"region,omitzero"`

	// Bucket is the bucket containing checkpoint files.
	Bucket string `json:"bucket"`

	// ObjectPrefix is prepended to checkpoint file names to form object keys.
	// Note that ObjectPrefix is prepended as a string, not a path, so if
	// ObjectPrefix does not have a trailing "/", no "/" will be inserted
	// between ObjectPrefix and the filename.
	ObjectPrefix string `json:"object_prefix"`

	// Addressing is how requests address the bucket: "path"
	// (https://host/bucket/key), as most stores other than AWS require, or
	// "virtual" (https://bucket.host/key). If it is empty, it is "path" with
	// an Endpoint, and "virtual" otherwise.
	Addressing string `json:"addressing,omitzero"`

	// If Credentials is not nil, it signs requests. Otherwise, the AWS SDK's
	// default chain provides credentials: the environment, the shared
	// credentials and configuration files (CredentialsFile and Profile, if
	// set, select them), web identity, and container and instance metadata.
	Credentials     *S3Credentials `json:"credentials,omitzero"`
	CredentialsFile string         `json:"credentials_file,omitzero"`
	Profile         string         `json:"profile,omitzero"`

	// MaxAttempts is the number of attempts of each request (default 5).
	MaxAttempts int `json:"max_attempts,omitzero"`

	// RequestTimeoutSeconds bounds each request, with its retries, plus the
	// time to transfer its bytes at 1 MiB/s (default 30).
	RequestTimeoutSeconds int `json:"request_timeout_seconds,omitzero"`

	// ReadBytes and ReadParallel are the size of reads of the pages file and
	// the number of reads in flight (default 16 MiB and 8).
	ReadBytes    uint64 `json:"read_bytes,omitzero"`
	ReadParallel int    `json:"read_parallel,omitzero"`

	// WritePartBytes and WriteParallel are the size of the parts in which the
	// pages file is uploaded, at least 5 MiB, and the number of parts
	// uploaded at once (default 32 MiB and 4).
	WritePartBytes int `json:"write_part_bytes,omitzero"`
	WriteParallel  int `json:"write_parallel,omitzero"`
}

// S3Credentials are static credentials for an S3-compatible object store.
type S3Credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitzero"`
}

// checkpointGoferCmd implements subcommands.Command for the checkpoint gofer.
type checkpointGoferCmd struct {
	util.InternalSubCommand

	allowCheckpointReads    bool
	allowCheckpointWrites   bool
	allowFSCheckpointReads  bool
	allowFSCheckpointWrites bool

	sockFD    int
	gcsOptsFD int
	s3OptsFD  int
}

// Name implements subcommands.Command.Name.
func (*checkpointGoferCmd) Name() string {
	return "checkpointgofer"
}

// Synopsis implements subcommands.Command.Synopsis.
func (*checkpointGoferCmd) Synopsis() string {
	return "runs process for remote checkpoint file access"
}

// Usage implements subcommands.Command.Usage.
func (*checkpointGoferCmd) Usage() string {
	return "[-allow-checkpoint-reads|-allow-checkpoint-writes|-allow-fscheckpoint-reads|-allow-fscheckpoint-writes] -sock-fd=<socket fd> -gcs-opts-fd=<options fd>|-s3-opts-fd=<options fd>\n"
}

// SetFlags implements subcommands.Command.SetFlags.
func (cmd *checkpointGoferCmd) SetFlags(f *flag.FlagSet) {
	f.BoolVar(&cmd.allowCheckpointReads, "allow-checkpoint-reads", false, "enable reading checkpoint files")
	f.BoolVar(&cmd.allowCheckpointWrites, "allow-checkpoint-writes", false, "enable writing checkpoint files")
	f.BoolVar(&cmd.allowFSCheckpointReads, "allow-fscheckpoint-reads", false, "enable reading filesystem checkpoint files")
	f.BoolVar(&cmd.allowFSCheckpointWrites, "allow-fscheckpoint-writes", false, "enable writing filesystem checkpoint files")
	f.IntVar(&cmd.sockFD, "sock-fd", -1, "FD for a Unix domain socket that is connected to the sentry")
	f.IntVar(&cmd.gcsOptsFD, "gcs-opts-fd", -1, "FD for a file containing GCSOptions in JSON")
	f.IntVar(&cmd.s3OptsFD, "s3-opts-fd", -1, "FD for a file containing S3Options in JSON")
}

// Execute implements subcommands.Command.Execute.
func (cmd *checkpointGoferCmd) Execute(ctx context.Context, f *flag.FlagSet, args ...any) subcommands.ExitStatus {
	if (!cmd.allowCheckpointReads && !cmd.allowCheckpointWrites && !cmd.allowFSCheckpointReads && !cmd.allowFSCheckpointWrites) || cmd.sockFD < 0 || (cmd.gcsOptsFD < 0) == (cmd.s3OptsFD < 0) {
		f.Usage()
		return subcommands.ExitUsageError
	}

	sock, err := unet.NewSocket(cmd.sockFD)
	if err != nil {
		util.Fatalf("Failed to construct unet.Socket: %v", err)
	}

	if cmd.gcsOptsFD >= 0 {
		var gcsOpts GCSOptions
		if _, err := decodeOptions(cmd.gcsOptsFD, "checkpointgofer.GCSOptions.json", &gcsOpts, false /* strict */); err != nil {
			util.Fatalf("%v", err)
		}
		err = cmd.runGCS(ctx, sock, &gcsOpts)
	} else {
		var (
			s3Opts S3Options
			fi     fs.FileInfo
		)
		if fi, err = decodeOptions(cmd.s3OptsFD, "checkpointgofer.S3Options.json", &s3Opts, true /* strict */); err != nil {
			util.Fatalf("%v", err)
		}
		// runsc opened the options file and passed it to this process: it
		// decides where checkpoints go and may hold credentials.
		if err := s3.CheckFileMode("the S3 options file", fi, s3Opts.Credentials != nil); err != nil {
			util.Fatalf("%v", err)
		}
		err = cmd.runS3(ctx, sock, &s3Opts)
	}
	if err != nil {
		util.Fatalf("%v", err)
	}

	util.Infof("Server stopped, exiting")
	return subcommands.ExitSuccess
}

// decodeOptions decodes the options in JSON in the file at fd, named name,
// into opts, closes the file, and returns its information. If strict is true,
// unknown (e.g. misspelled) options are an error.
func decodeOptions(fd int, name string, opts any, strict bool) (fs.FileInfo, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat options: %w", err)
	}
	dec := json.NewDecoder(f)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(opts); err != nil {
		return nil, fmt.Errorf("failed to decode options: %w", err)
	}
	return fi, nil
}

// runGCS runs the checkpoint gofer for GCS access on the given socket. It does
// not return until the client disconnects or a fatal error occurs.
func (cmd *checkpointGoferCmd) runGCS(ctx context.Context, sock *unet.Socket, opts *GCSOptions) error {
	if len(opts.Bucket) == 0 {
		return fmt.Errorf("bucket must be specified")
	}

	gcsfsOpts := gcs.FileServerOptions{
		AllowCheckpointReads:    cmd.allowCheckpointReads,
		AllowCheckpointWrites:   cmd.allowCheckpointWrites,
		AllowFSCheckpointReads:  cmd.allowFSCheckpointReads,
		AllowFSCheckpointWrites: cmd.allowFSCheckpointWrites,
		Bucket:                  opts.Bucket,
		ObjectPrefix:            opts.ObjectPrefix,
	}
	if opts.Token != nil {
		gcsfsOpts.TokenSource = oauth2.StaticTokenSource(opts.Token)
	}
	switch opts.ParallelCompositeUpload {
	case "", "safe":
		gcsfsOpts.ParallelCompositeUpload = gcs.ParallelCompositeUploadSafe
	case "disable":
		gcsfsOpts.ParallelCompositeUpload = gcs.ParallelCompositeUploadDisable
	case "force":
		gcsfsOpts.ParallelCompositeUpload = gcs.ParallelCompositeUploadForce
	default:
		return fmt.Errorf("unknown parallel_composite_upload value %q", opts.ParallelCompositeUpload)
	}

	gcsfs, err := gcs.NewFileServer(ctx, &gcsfsOpts)
	if err != nil {
		return fmt.Errorf("failed to construct GCS file server: %w", err)
	}
	afs, err := stateipc.NewAsyncFileServer(gcsfs)
	if err != nil {
		return fmt.Errorf("failed to construct stateipc server: %w", err)
	}
	server := urpc.NewServer()
	server.Register(afs)
	server.Handle(sock)
	return nil
}

// runS3 runs the checkpoint gofer for access to an S3-compatible object store
// on the given socket. It does not return until the client disconnects or a
// fatal error occurs.
func (cmd *checkpointGoferCmd) runS3(ctx context.Context, sock *unet.Socket, opts *S3Options) error {
	if len(opts.Bucket) == 0 {
		return fmt.Errorf("bucket must be specified")
	}
	s3fsOpts := s3.FileServerOptions{
		AllowCheckpointReads:    cmd.allowCheckpointReads,
		AllowCheckpointWrites:   cmd.allowCheckpointWrites,
		AllowFSCheckpointReads:  cmd.allowFSCheckpointReads,
		AllowFSCheckpointWrites: cmd.allowFSCheckpointWrites,
		Endpoint:                opts.Endpoint,
		Region:                  opts.Region,
		Bucket:                  opts.Bucket,
		ObjectPrefix:            opts.ObjectPrefix,
		SharedCredentialsFile:   opts.CredentialsFile,
		Profile:                 opts.Profile,
		MaxAttempts:             opts.MaxAttempts,
		RequestTimeout:          time.Duration(opts.RequestTimeoutSeconds) * time.Second,
		PagesFileReadBytes:      opts.ReadBytes,
		PagesFileReadParallel:   opts.ReadParallel,
		PagesFilePartBytes:      opts.WritePartBytes,
		PagesFileWriteParallel:  opts.WriteParallel,
	}
	switch opts.Addressing {
	case "":
		s3fsOpts.UsePathStyle = opts.Endpoint != ""
	case "path":
		s3fsOpts.UsePathStyle = true
	case "virtual":
		s3fsOpts.UsePathStyle = false
	default:
		return fmt.Errorf("unknown addressing value %q", opts.Addressing)
	}
	if c := opts.Credentials; c != nil {
		if c.AccessKeyID == "" || c.SecretAccessKey == "" {
			return fmt.Errorf("credentials must have an access_key_id and a secret_access_key")
		}
		if opts.CredentialsFile != "" || opts.Profile != "" {
			return fmt.Errorf("credentials cannot be given with credentials_file or profile")
		}
		s3fsOpts.Credentials = credentials.NewStaticCredentialsProvider(c.AccessKeyID, c.SecretAccessKey, c.SessionToken)
	}

	s3fs, err := s3.NewFileServer(ctx, &s3fsOpts)
	if err != nil {
		return fmt.Errorf("failed to construct S3 file server: %w", err)
	}
	afs, err := stateipc.NewAsyncFileServer(s3fs)
	if err != nil {
		return fmt.Errorf("failed to construct stateipc server: %w", err)
	}
	server := urpc.NewServer()
	server.Register(afs)
	server.Handle(sock)
	return nil
}

// main is the binary's entry point.
func main() {
	cli.Run(&gvisorbinaries.CheckpointGofer, map[util.SubCommand]string{
		new(checkpointGoferCmd): "",
	}, nil)
}
