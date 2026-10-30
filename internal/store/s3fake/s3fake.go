// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package s3fake is an in-memory stand-in for the parts of S3 that vmobj
// uses. It exists for unit tests, so they run anywhere with no network and
// no account. Behaviour that matters to vmobj is copied from real S3,
// including its error codes; anything vmobj doesn't use is left out.
//
// The fake is checked against real S3-compatible storage by the shared
// conformance tests in internal/store, which CI also runs against MinIO.
package s3fake

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Object is one stored object, exposed so tests can inspect or damage it.
type Object struct {
	Data     []byte
	Modified time.Time
	Class    types.StorageClass // "" means Standard, as in S3
	// Restore state for archived classes.
	Restoring     bool
	RestoreDays   int
	RestoreTier   types.Tier
	RestoredUntil time.Time
}

func (o *Object) archived() bool {
	return o.Class == types.StorageClassGlacier || o.Class == types.StorageClassDeepArchive
}

// readable applies S3's rule: archived objects can be read only while a
// finished restore's copy exists.
func (o *Object) readable(now time.Time) bool {
	return !o.archived() || (!o.Restoring && now.Before(o.RestoredUntil))
}

// knownClasses are the classes the fake accepts. Set Server.Classes to
// imitate a service that knows fewer (MinIO and QuObjects know Standard).
var knownClasses = []types.StorageClass{
	"", types.StorageClassStandard, types.StorageClassIntelligentTiering, types.StorageClassStandardIa,
	types.StorageClassOnezoneIa, types.StorageClassGlacierIr, types.StorageClassGlacier, types.StorageClassDeepArchive,
}

// Server holds the buckets. The zero value is not usable; call New.
//
// Go note: sync.Mutex guards the map, because the engine uploads from
// several goroutines at once and Go maps are not safe for concurrent writes.
type Server struct {
	mu      sync.Mutex
	buckets map[string]map[string]*Object

	// PageSize is how many keys ListObjectsV2 returns per page. It is small
	// by default so tests exercise the continuation-token loop.
	PageSize int
	// Now is the clock; tests may replace it.
	Now func() time.Time
	// Calls counts requests by operation name, for tests that check
	// efficiency (for example that a ranged read was used).
	Calls map[string]int
	// Classes, when set, limits which storage classes PutObject accepts.
	Classes []types.StorageClass
}

// New returns a server with the named buckets already created.
func New(buckets ...string) *Server {
	s := &Server{buckets: map[string]map[string]*Object{}, PageSize: 3, Now: time.Now, Calls: map[string]int{}}
	for _, b := range buckets {
		s.buckets[b] = map[string]*Object{}
	}
	return s
}

// Object returns a stored object, or nil.
func (s *Server) Object(bucket, key string) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buckets[bucket][key]
}

// apiErr builds an error shaped like the SDK's own decoded S3 errors.
func apiErr(code, msg string) error {
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

// bucket returns the named bucket's map. The caller holds s.mu.
func (s *Server) bucket(name *string) (map[string]*Object, error) {
	b, ok := s.buckets[aws.ToString(name)]
	if !ok {
		return nil, apiErr("NoSuchBucket", "The specified bucket does not exist")
	}
	return b, nil
}

// PutObject stores an object, checking Content-MD5 when one is sent.
func (s *Server) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if in.ContentLength != nil && *in.ContentLength != int64(len(data)) {
		return nil, apiErr("IncompleteBody", "body length does not match Content-Length")
	}
	if in.ContentMD5 != nil {
		sum := md5.Sum(data)
		if base64.StdEncoding.EncodeToString(sum[:]) != *in.ContentMD5 {
			return nil, apiErr("BadDigest", "The Content-MD5 you specified did not match what we received")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["PutObject"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	allowed := s.Classes
	if allowed == nil {
		allowed = knownClasses
	}
	if !slices.Contains(allowed, in.StorageClass) {
		return nil, apiErr("InvalidStorageClass", "The storage class you specified is not valid")
	}
	class := in.StorageClass
	if class == types.StorageClassStandard {
		class = ""
	}
	b[aws.ToString(in.Key)] = &Object{Data: data, Modified: s.Now(), Class: class}
	return &s3.PutObjectOutput{}, nil
}

// GetObject returns an object or a byte range of it.
func (s *Server) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["GetObject"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	o, ok := b[aws.ToString(in.Key)]
	if !ok {
		return nil, apiErr("NoSuchKey", "The specified key does not exist.")
	}
	if !o.readable(s.Now()) {
		return nil, apiErr("InvalidObjectState", "The operation is not valid for the object's storage class")
	}
	data := o.Data
	if in.Range != nil {
		s.Calls["GetObject.Range"]++
		start, end, err := parseRange(*in.Range, int64(len(data)))
		if err != nil {
			return nil, err
		}
		data = data[start:end]
	}
	// Copy, so a caller can't change the stored object through the slice.
	body := bytes.Clone(data)
	return &s3.GetObjectOutput{
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: aws.Int64(int64(len(body))),
		LastModified:  aws.Time(o.Modified),
	}, nil
}

// parseRange handles the one form vmobj sends, "bytes=FIRST-LAST", with
// S3's rules: LAST past the end is shortened, FIRST past the end is an
// InvalidRange error.
func parseRange(r string, size int64) (int64, int64, error) {
	spec, ok := strings.CutPrefix(r, "bytes=")
	first, last, ok2 := strings.Cut(spec, "-")
	a, err1 := strconv.ParseInt(first, 10, 64)
	b, err2 := strconv.ParseInt(last, 10, 64)
	if !ok || !ok2 || err1 != nil || err2 != nil || a < 0 || b < a {
		return 0, 0, fmt.Errorf("s3fake: unsupported range %q", r)
	}
	if a >= size {
		return 0, 0, apiErr("InvalidRange", "The requested range is not satisfiable")
	}
	return a, min(b+1, size), nil
}

// HeadObject reports whether an object exists. Like real S3 it has no
// error body, so a missing key comes back as the bare code "NotFound".
func (s *Server) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["HeadObject"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	o, ok := b[aws.ToString(in.Key)]
	if !ok {
		return nil, apiErr("NotFound", "Not Found")
	}
	out := &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(o.Data))), LastModified: aws.Time(o.Modified),
		StorageClass: o.Class}
	switch {
	case o.Restoring:
		out.Restore = aws.String(`ongoing-request="true"`)
	case !o.RestoredUntil.IsZero() && s.Now().Before(o.RestoredUntil):
		out.Restore = aws.String(fmt.Sprintf(`ongoing-request="false", expiry-date="%s"`,
			o.RestoredUntil.UTC().Format(http.TimeFormat)))
	}
	return out, nil
}

// RestoreObject starts a restore of an archived object. Call
// CompleteRestores to finish the running ones, as S3 would hours later.
func (s *Server) RestoreObject(ctx context.Context, in *s3.RestoreObjectInput, _ ...func(*s3.Options)) (*s3.RestoreObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["RestoreObject"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	o, ok := b[aws.ToString(in.Key)]
	if !ok {
		return nil, apiErr("NoSuchKey", "The specified key does not exist.")
	}
	if !o.archived() {
		return nil, apiErr("InvalidObjectState", "Restore is not allowed for the object's current storage class")
	}
	if o.Restoring {
		return nil, apiErr("RestoreAlreadyInProgress", "Object restore is already in progress")
	}
	r := in.RestoreRequest
	if r == nil || aws.ToInt32(r.Days) < 1 {
		return nil, apiErr("MalformedXML", "restore request needs Days")
	}
	tier := types.TierStandard
	if r.GlacierJobParameters != nil && r.GlacierJobParameters.Tier != "" {
		tier = r.GlacierJobParameters.Tier
	}
	if o.Class == types.StorageClassDeepArchive && tier == types.TierExpedited {
		return nil, apiErr("InvalidArgument", "Expedited retrieval is not available for Deep Archive")
	}
	o.Restoring, o.RestoreDays, o.RestoreTier = true, int(aws.ToInt32(r.Days)), tier
	return &s3.RestoreObjectOutput{}, nil
}

// CompleteRestores finishes every running restore, returning how many.
func (s *Server) CompleteRestores() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, b := range s.buckets {
		for _, o := range b {
			if o.Restoring {
				o.Restoring = false
				o.RestoredUntil = s.Now().Add(time.Duration(o.RestoreDays) * 24 * time.Hour)
				n++
			}
		}
	}
	return n
}

// ListObjectsV2 lists keys in order, PageSize at a time.
func (s *Server) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["ListObjectsV2"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range b {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	// The token is simply the last key already returned.
	if tok := aws.ToString(in.ContinuationToken); tok != "" {
		i := sort.SearchStrings(keys, tok)
		if i < len(keys) && keys[i] == tok {
			i++
		}
		keys = keys[i:]
	}
	out := &s3.ListObjectsV2Output{}
	page := s.PageSize
	if page <= 0 {
		page = 1000
	}
	if len(keys) > page {
		keys = keys[:page]
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(keys[len(keys)-1])
	} else {
		out.IsTruncated = aws.Bool(false)
	}
	for _, k := range keys {
		class := types.ObjectStorageClass(b[k].Class)
		if class == "" {
			class = types.ObjectStorageClassStandard
		}
		out.Contents = append(out.Contents, types.Object{
			Key:          aws.String(k),
			Size:         aws.Int64(int64(len(b[k].Data))),
			LastModified: aws.Time(b[k].Modified),
			StorageClass: class,
		})
	}
	out.KeyCount = aws.Int32(int32(len(out.Contents)))
	return out, nil
}

// DeleteObject removes an object. Missing keys are not an error, as in S3.
func (s *Server) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Calls["DeleteObject"]++
	b, err := s.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	delete(b, aws.ToString(in.Key))
	return &s3.DeleteObjectOutput{}, nil
}
