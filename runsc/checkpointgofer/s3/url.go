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

package s3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
)

// urlsCheckEvery is how often, at most, a urlSource whose URLs near their
// expiry looks at its file for URLs signed again. It is a variable for tests.
var urlsCheckEvery = time.Second

// urlSource holds the URLs of a FileServer that reads by URLs
// (FileServerOptions.URLsFile), as its file says them, read again as they
// near their expiry, and on a refusal, where the file has changed since.
type urlSource struct {
	path string

	mu sync.Mutex
	// urls holds the URL of each path, as the file said last.
	urls map[string]*url.URL
	// renew is when a URL has a third of its life left, the earliest of
	// them: past it, get looks at the file again, at most every
	// urlsCheckEvery, from checked on. It is zero where no URL says when it
	// expires.
	renew   time.Time
	checked time.Time
	// read identifies the file as it was read last: its inode and its
	// modification time.
	read fileStamp
}

// fileStamp identifies a version of a file that is replaced whole.
type fileStamp struct {
	ino   uint64
	mtime int64
	size  int64
}

// stampOf returns the stamp of the file fi describes.
func stampOf(fi os.FileInfo) fileStamp {
	st, _ := fi.Sys().(*syscall.Stat_t)
	stamp := fileStamp{mtime: fi.ModTime().UnixNano(), size: fi.Size()}
	if st != nil {
		stamp.ino = st.Ino
	}
	return stamp
}

// load reads the file, which must be readable by its owner alone, as
// credentials are, and each of whose URLs must be an absolute HTTP(S) URL.
func (u *urlSource) load() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.loadLocked()
}

// Preconditions: u.mu must be locked.
func (u *urlSource) loadLocked() error {
	f, err := os.Open(u.path)
	if err != nil {
		return fmt.Errorf("the URLs file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("the URLs file: %w", err)
	}
	if err := CheckFileMode("the URLs file", fi, true /* secret */); err != nil {
		return err
	}
	var raw map[string]string
	if err := json.NewDecoder(f).Decode(&raw); err != nil {
		// Not err's text alone, which may quote the file.
		return fmt.Errorf("the URLs file %s is not a JSON object of URLs", u.path)
	}
	urls := make(map[string]*url.URL, len(raw))
	var renew time.Time
	for p, s := range raw {
		parsed, err := url.Parse(s)
		// Not err, nor s, which would print the URL's query.
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return fmt.Errorf("the URL of %q is not an absolute HTTP(S) URL", p)
		}
		urls[p] = parsed
		if at, ok := renewalOf(parsed); ok && (renew.IsZero() || at.Before(renew)) {
			renew = at
		}
	}
	u.urls, u.renew, u.read = urls, renew, stampOf(fi)
	return nil
}

// renewalOf returns when a URL presigned with Signature Version 4 has a third
// of its life left (X-Amz-Date, X-Amz-Expires), if it says.
func renewalOf(u *url.URL) (time.Time, bool) {
	q := u.Query()
	signed, err := time.Parse("20060102T150405Z", q.Get("X-Amz-Date"))
	if err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	life := time.Duration(seconds) * time.Second
	return signed.Add(life - life/3), true
}

// has returns whether the file has a URL of path.
func (u *urlSource) has(path string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, ok := u.urls[path]
	return ok
}

// get returns the URL of path, the file read again first where its URLs near
// their expiry and it has changed since.
func (u *urlSource) get(path string) (*url.URL, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if now := time.Now(); !u.renew.IsZero() && now.After(u.renew) && now.Sub(u.checked) >= urlsCheckEvery {
		u.checked = now
		u.reloadIfChangedLocked()
	}
	found, ok := u.urls[path]
	return found, ok
}

// reloadIfChanged reads the file again if it has changed since it was read
// last, and returns whether it did.
func (u *urlSource) reloadIfChanged() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.reloadIfChangedLocked()
}

// Preconditions: u.mu must be locked.
func (u *urlSource) reloadIfChangedLocked() bool {
	fi, err := os.Stat(u.path)
	if err != nil || stampOf(fi) == u.read {
		return false
	}
	if err := u.loadLocked(); err != nil {
		log.Warningf("s3: cannot read the URLs again: %v", err)
		return false
	}
	log.Infof("s3: read the URLs again, as their host signed them again")
	return true
}

// refused returns whether err is the store's refusal of a request's
// authorization (403 or 401), as of a URL past its expiry.
func refused(err error) bool {
	var respErr *awshttp.ResponseError
	return errors.As(err, &respErr) && (respErr.HTTPStatusCode() == http.StatusForbidden || respErr.HTTPStatusCode() == http.StatusUnauthorized)
}

// byURL returns the option of a GetObject that makes its request to u, a URL
// presigned for it (FileServerOptions.URLs), rather than to the object's URL
// at the store's endpoint: as the last step of the request's finalization, on
// each of its attempts, its URL is replaced by u, with u's query, which signs
// it, and none of the client's own query (such as x-id), and the headers of
// the store's that the client set (x-amz-*) are removed, as u's signature
// does not sign them; Range and If-Match are kept. An error that names the
// request's URL names it without its query.
func byURL(u *url.URL) func(*s3.Options) {
	return func(o *s3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Finalize.Add(presignedRequest{url: u}, middleware.After)
		})
	}
}

// presignedRequest is the middleware of byURL.
type presignedRequest struct {
	url *url.URL
}

// ID implements middleware.FinalizeMiddleware.ID.
func (presignedRequest) ID() string {
	return "gVisorPresignedURL"
}

// HandleFinalize implements middleware.FinalizeMiddleware.HandleFinalize.
func (m presignedRequest) HandleFinalize(ctx context.Context, in middleware.FinalizeInput, next middleware.FinalizeHandler) (middleware.FinalizeOutput, middleware.Metadata, error) {
	req, ok := in.Request.(*smithyhttp.Request)
	if !ok {
		return middleware.FinalizeOutput{}, middleware.Metadata{}, fmt.Errorf("unexpected request type %T", in.Request)
	}
	u := *m.url
	req.URL = &u
	req.Host = ""
	for name := range req.Header {
		if lower := strings.ToLower(name); lower == "authorization" || strings.HasPrefix(lower, "x-amz-") {
			req.Header.Del(name)
		}
	}
	out, md, err := next.HandleFinalize(ctx, in)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = redactedURL(m.url)
	}
	return out, md, err
}

// redactedURL returns u without its query, which grants what it signs.
func redactedURL(u *url.URL) string {
	r := *u
	r.RawQuery = ""
	r.ForceQuery = false
	r.Fragment = ""
	r.RawFragment = ""
	r.User = nil
	return r.String()
}
