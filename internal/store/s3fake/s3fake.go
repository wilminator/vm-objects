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
// Modelled: storage classes and archive restores, versioned buckets with
// delete markers, and Object Lock in Governance mode (including that a
// lock can be extended but not shortened, and that a locked version can't
// be deleted without the bypass header).
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

// Object is one stored version, exposed so tests can inspect or damage it.
type Object struct {
	Data         []byte
	Modified     time.Time
	VersionID    string
	DeleteMarker bool
	Class        types.StorageClass // "" means Standard, as in S3
	// Restore state for archived classes.
	Restoring     bool
	RestoreDays   int
	RestoreTier   types.Tier
	RestoredUntil time.Time
	// Object Lock.
	LockMode  types.ObjectLockMode
	LockUntil time.Time
}

func (o *Object) archived() bool {
	return o.Class == types.StorageClassGlacier || o.Class == types.StorageClassDeepArchive
}

// readable applies S3's rule: archived objects can be read only while a
// finished restore's copy exists.
func (o *Object) readable(now time.Time) bool {
	return !o.archived() || (!o.Restoring && now.Before(o.RestoredUntil))
}

func (o *Object) locked(now time.Time) bool { return o.LockMode != "" && now.Before(o.LockUntil) }

// bucket holds every version of every key, oldest first.
type bucket struct {
	versioned bool
	locking   bool
	keys      map[string][]*Object
}

// latest returns a key's current version, or nil if the key is missing or
// its current version is a delete marker.
func (b *bucket) latest(key string) *Object {
	vs := b.keys[key]
	if len(vs) == 0 || vs[len(vs)-1].DeleteMarker {
		return nil
	}
	return vs[len(vs)-1]
}

func (b *bucket) version(key, id string) *Object {
	for _, o := range b.keys[key] {
		if o.VersionID == id {
			return o
		}
	}
	return nil
}

// knownClasses are the classes the fake accepts. Set Server.Classes to
// imitate a service that knows fewer (MinIO and QuObjects know Standard).
var knownClasses = []types.StorageClass{
	"", types.StorageClassStandard, types.StorageClassIntelligentTiering, types.StorageClassStandardIa,
	types.StorageClassOnezoneIa, types.StorageClassGlacierIr, types.StorageClassGlacier, types.StorageClassDeepArchive,
}

// Server holds the buckets. The zero value is not usable; call New.
//
// Go note: sync.Mutex guards the maps, because the engine uploads from
// several goroutines at once and Go maps are not safe for concurrent writes.
type Server struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	nextID  int

	// PageSize is how many entries list calls return per page. It is small
	// by default so tests exercise the continuation loops.
	PageSize int
	// Now is the clock; tests may replace it.
	Now func() time.Time
	// Calls counts requests by operation name, for tests that check
	// efficiency (for example that a ranged read was used).
	Calls map[string]int
	// Classes, when set, limits which storage classes PutObject accepts.
	Classes []types.StorageClass
}

// New returns a server with the named plain (unversioned) buckets.
func New(buckets ...string) *Server {
	s := &Server{buckets: map[string]*bucket{}, PageSize: 3, Now: time.Now, Calls: map[string]int{}}
	for _, b := range buckets {
		s.buckets[b] = &bucket{keys: map[string][]*Object{}}
	}
	return s
}

// AddLockedBucket creates a bucket with versioning and Object Lock on, as
// "aws s3api create-bucket --object-lock-enabled-for-bucket" does.
func (s *Server) AddLockedBucket(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[name] = &bucket{versioned: true, locking: true, keys: map[string][]*Object{}}
}

// Object returns a key's current version, or nil.
func (s *Server) Object(bucketName, key string) *Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[bucketName]; b != nil {
		return b.latest(key)
	}
	return nil
}

// Versions returns every version of a key, oldest first.
func (s *Server) Versions(bucketName, key string) []*Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.buckets[bucketName]; b != nil {
		return slices.Clone(b.keys[key])
	}
	return nil
}

// apiErr builds an error shaped like the SDK's own decoded S3 errors.
func apiErr(code, msg string) error {
	return &smithy.GenericAPIError{Code: code, Message: msg}
}

// begin checks the context, takes the lock and counts the call. The
// caller must call s.mu.Unlock (by defer) when begin returns no error.
func (s *Server) begin(ctx context.Context, op string, name *string) (*bucket, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.Calls[op]++
	b, ok := s.buckets[aws.ToString(name)]
	if !ok {
		s.mu.Unlock()
		return nil, apiErr("NoSuchBucket", "The specified bucket does not exist")
	}
	return b, nil
}

func (s *Server) newVersionID() string {
	s.nextID++
	return fmt.Sprintf("v%06d", s.nextID)
}

// PutObject stores an object, checking Content-MD5 when one is sent.
func (s *Server) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
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
	b, err := s.begin(ctx, "PutObject", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	allowed := s.Classes
	if allowed == nil {
		allowed = knownClasses
	}
	if !slices.Contains(allowed, in.StorageClass) {
		return nil, apiErr("InvalidStorageClass", "The storage class you specified is not valid")
	}
	now := s.Now()
	o := &Object{Data: data, Modified: now, Class: in.StorageClass}
	if o.Class == types.StorageClassStandard {
		o.Class = ""
	}
	if in.ObjectLockMode != "" || in.ObjectLockRetainUntilDate != nil {
		switch {
		case !b.locking:
			return nil, apiErr("InvalidRequest", "Bucket is missing Object Lock Configuration")
		case in.ContentMD5 == nil:
			return nil, apiErr("InvalidRequest", "Content-MD5 OR x-amz-checksum- HTTP header is required for Put Object requests with Object Lock parameters")
		case in.ObjectLockMode == "" || in.ObjectLockRetainUntilDate == nil:
			return nil, apiErr("InvalidRequest", "x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
		case !in.ObjectLockRetainUntilDate.After(now):
			return nil, apiErr("InvalidArgument", "The retain until date must be in the future")
		}
		o.LockMode, o.LockUntil = in.ObjectLockMode, *in.ObjectLockRetainUntilDate
	}
	key := aws.ToString(in.Key)
	if b.versioned {
		o.VersionID = s.newVersionID()
		b.keys[key] = append(b.keys[key], o)
	} else {
		o.VersionID = "null"
		b.keys[key] = []*Object{o}
	}
	return &s3.PutObjectOutput{VersionId: aws.String(o.VersionID)}, nil
}

// find returns the version asked for (or the current one), or an error
// shaped like S3's.
func find(b *bucket, key string, versionID *string, missing string) (*Object, error) {
	var o *Object
	if versionID != nil {
		o = b.version(key, *versionID)
	} else {
		o = b.latest(key)
	}
	if o == nil || o.DeleteMarker {
		return nil, apiErr(missing, "The specified key does not exist.")
	}
	return o, nil
}

// GetObject returns an object or a byte range of it.
func (s *Server) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b, err := s.begin(ctx, "GetObject", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	o, err := find(b, aws.ToString(in.Key), in.VersionId, "NoSuchKey")
	if err != nil {
		return nil, err
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
		VersionId:     aws.String(o.VersionID),
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

// HeadObject reports an object's headers. Like real S3 it has no error
// body, so a missing key comes back as the bare code "NotFound".
func (s *Server) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	b, err := s.begin(ctx, "HeadObject", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	o, err := find(b, aws.ToString(in.Key), in.VersionId, "NotFound")
	if err != nil {
		return nil, err
	}
	out := &s3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(o.Data))), LastModified: aws.Time(o.Modified),
		StorageClass: o.Class, VersionId: aws.String(o.VersionID)}
	if o.LockMode != "" {
		out.ObjectLockMode = o.LockMode
		out.ObjectLockRetainUntilDate = aws.Time(o.LockUntil)
	}
	switch {
	case o.Restoring:
		out.Restore = aws.String(`ongoing-request="true"`)
	case !o.RestoredUntil.IsZero() && s.Now().Before(o.RestoredUntil):
		out.Restore = aws.String(fmt.Sprintf(`ongoing-request="false", expiry-date="%s"`,
			o.RestoredUntil.UTC().Format(http.TimeFormat)))
	}
	return out, nil
}

// ListObjectsV2 lists current, non-deleted keys in order, PageSize at a time.
func (s *Server) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	b, err := s.begin(ctx, "ListObjectsV2", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range b.keys {
		if strings.HasPrefix(k, prefix) && b.latest(k) != nil {
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
	out := &s3.ListObjectsV2Output{IsTruncated: aws.Bool(false)}
	if page := s.pageSize(); len(keys) > page {
		keys = keys[:page]
		out.IsTruncated = aws.Bool(true)
		out.NextContinuationToken = aws.String(keys[len(keys)-1])
	}
	for _, k := range keys {
		o := b.latest(k)
		class := types.ObjectStorageClass(o.Class)
		if class == "" {
			class = types.ObjectStorageClassStandard
		}
		out.Contents = append(out.Contents, types.Object{
			Key: aws.String(k), Size: aws.Int64(int64(len(o.Data))),
			LastModified: aws.Time(o.Modified), StorageClass: class,
		})
	}
	out.KeyCount = aws.Int32(int32(len(out.Contents)))
	return out, nil
}

func (s *Server) pageSize() int {
	if s.PageSize <= 0 {
		return 1000
	}
	return s.PageSize
}

// ListObjectVersions lists every version and delete marker, newest first
// within each key as S3 does, PageSize entries at a time.
func (s *Server) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	b, err := s.begin(ctx, "ListObjectVersions", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	prefix := aws.ToString(in.Prefix)
	var keys []string
	for k := range b.keys {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	type entry struct {
		key    string
		o      *Object
		latest bool
	}
	var all []entry
	for _, k := range keys {
		vs := b.keys[k]
		for i := len(vs) - 1; i >= 0; i-- {
			all = append(all, entry{k, vs[i], i == len(vs)-1})
		}
	}
	// Resume after the marker pair.
	if km := aws.ToString(in.KeyMarker); km != "" {
		vm := aws.ToString(in.VersionIdMarker)
		for i, e := range all {
			if e.key == km && e.o.VersionID == vm {
				all = all[i+1:]
				break
			}
		}
	}
	out := &s3.ListObjectVersionsOutput{IsTruncated: aws.Bool(false)}
	if page := s.pageSize(); len(all) > page {
		all = all[:page]
		last := all[len(all)-1]
		out.IsTruncated = aws.Bool(true)
		out.NextKeyMarker, out.NextVersionIdMarker = aws.String(last.key), aws.String(last.o.VersionID)
	}
	for _, e := range all {
		if e.o.DeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, types.DeleteMarkerEntry{
				Key: aws.String(e.key), VersionId: aws.String(e.o.VersionID),
				IsLatest: aws.Bool(e.latest), LastModified: aws.Time(e.o.Modified),
			})
			continue
		}
		out.Versions = append(out.Versions, types.ObjectVersion{
			Key: aws.String(e.key), VersionId: aws.String(e.o.VersionID), IsLatest: aws.Bool(e.latest),
			LastModified: aws.Time(e.o.Modified), Size: aws.Int64(int64(len(e.o.Data))),
		})
	}
	return out, nil
}

// DeleteObject without a version hides the key behind a delete marker in a
// versioned bucket, or removes it in a plain one. With a version it
// removes that version, unless Object Lock protects it. The fake never
// honours a bypass, because vmobj never sends one.
func (s *Server) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	b, err := s.begin(ctx, "DeleteObject", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	key := aws.ToString(in.Key)
	if in.BypassGovernanceRetention != nil && *in.BypassGovernanceRetention {
		return nil, fmt.Errorf("s3fake: vmobj must never send the governance bypass header")
	}
	if in.VersionId != nil {
		vs := b.keys[key]
		for i, o := range vs {
			if o.VersionID != *in.VersionId {
				continue
			}
			if o.locked(s.Now()) {
				return nil, apiErr("AccessDenied", "Access Denied because object protected by object lock.")
			}
			b.keys[key] = slices.Delete(vs, i, i+1)
			if len(b.keys[key]) == 0 {
				delete(b.keys, key)
			}
			return &s3.DeleteObjectOutput{}, nil
		}
		return &s3.DeleteObjectOutput{}, nil
	}
	if !b.versioned {
		delete(b.keys, key)
		return &s3.DeleteObjectOutput{}, nil
	}
	if b.latest(key) == nil {
		return &s3.DeleteObjectOutput{}, nil
	}
	m := &Object{DeleteMarker: true, Modified: s.Now(), VersionID: s.newVersionID()}
	b.keys[key] = append(b.keys[key], m)
	return &s3.DeleteObjectOutput{DeleteMarker: aws.Bool(true), VersionId: aws.String(m.VersionID)}, nil
}

// PutObjectRetention sets or extends a version's lock. Shortening a
// Governance lock needs the bypass header, which vmobj never sends.
func (s *Server) PutObjectRetention(ctx context.Context, in *s3.PutObjectRetentionInput, _ ...func(*s3.Options)) (*s3.PutObjectRetentionOutput, error) {
	b, err := s.begin(ctx, "PutObjectRetention", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	if !b.locking {
		return nil, apiErr("InvalidRequest", "Bucket is missing Object Lock Configuration")
	}
	if in.BypassGovernanceRetention != nil && *in.BypassGovernanceRetention {
		return nil, fmt.Errorf("s3fake: vmobj must never send the governance bypass header")
	}
	o, err := find(b, aws.ToString(in.Key), in.VersionId, "NoSuchKey")
	if err != nil {
		return nil, err
	}
	r := in.Retention
	if r == nil || r.RetainUntilDate == nil || r.Mode == "" {
		return nil, apiErr("MalformedXML", "retention needs a mode and a date")
	}
	if o.locked(s.Now()) && r.RetainUntilDate.Before(o.LockUntil) {
		return nil, apiErr("AccessDenied", "Access Denied because object protected by object lock.")
	}
	o.LockMode, o.LockUntil = types.ObjectLockMode(r.Mode), *r.RetainUntilDate
	return &s3.PutObjectRetentionOutput{}, nil
}

// GetObjectLockConfiguration reports whether the bucket has Object Lock.
func (s *Server) GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, _ ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	b, err := s.begin(ctx, "GetObjectLockConfiguration", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	if !b.locking {
		return nil, apiErr("ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
	}
	return &s3.GetObjectLockConfigurationOutput{ObjectLockConfiguration: &types.ObjectLockConfiguration{
		ObjectLockEnabled: types.ObjectLockEnabledEnabled}}, nil
}

// RestoreObject starts a restore of an archived object. Call
// CompleteRestores to finish the running ones, as S3 would hours later.
func (s *Server) RestoreObject(ctx context.Context, in *s3.RestoreObjectInput, _ ...func(*s3.Options)) (*s3.RestoreObjectOutput, error) {
	b, err := s.begin(ctx, "RestoreObject", in.Bucket)
	if err != nil {
		return nil, err
	}
	defer s.mu.Unlock()
	o, err := find(b, aws.ToString(in.Key), in.VersionId, "NoSuchKey")
	if err != nil {
		return nil, err
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
		for _, vs := range b.keys {
			for _, o := range vs {
				if o.Restoring {
					o.Restoring = false
					o.RestoredUntil = s.Now().Add(time.Duration(o.RestoreDays) * 24 * time.Hour)
					n++
				}
			}
		}
	}
	return n
}
