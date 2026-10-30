// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// S3API is the part of the AWS S3 client that vmobj uses.
//
// Go note: an interface lists methods, and any type that has those methods
// satisfies it automatically, with no "implements" declaration. The real
// *s3.Client from the AWS SDK already has these methods, and so does the
// in-memory fake in internal/store/s3fake, so tests can swap one for the
// other without touching the backend code.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, opts ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
}

// S3 keeps objects in an S3 bucket (AWS, or a compatible service such as
// QNAP QuObjects or MinIO), optionally under a key prefix so several
// repositories can share one bucket.
type S3 struct {
	api    S3API
	bucket string
	prefix string // "" or ends with "/"
}

// NewS3 wraps an S3 client. prefix may be empty.
func NewS3(api S3API, bucket, prefix string) (*S3, error) {
	if bucket == "" {
		return nil, errors.New("s3: bucket name is required")
	}
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		if err := CheckKey(prefix); err != nil {
			return nil, fmt.Errorf("s3: bad prefix: %w", err)
		}
		prefix += "/"
	}
	return &S3{api: api, bucket: bucket, prefix: prefix}, nil
}

// String names the location, for messages.
func (s *S3) String() string { return "s3://" + s.bucket + "/" + s.prefix }

func (s *S3) key(k string) (*string, error) {
	if err := CheckKey(k); err != nil {
		return nil, err
	}
	return aws.String(s.prefix + k), nil
}

// Put uploads an object in one request. S3 replaces objects atomically:
// a reader sees the old object or the new one, never a mix.
//
// Content-MD5 is sent so S3 rejects a body damaged in transit. It is also
// required by S3 for uploads to buckets with Object Lock, and unlike the
// newer checksum headers every S3-compatible service understands it.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	sum := md5.Sum(data)
	_, err = s.api.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           k,
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentMD5:    aws.String(base64.StdEncoding.EncodeToString(sum[:])),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

// Get downloads a whole object.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	k, err := s.key(key)
	if err != nil {
		return nil, err
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: k})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer out.Body.Close()
	return io.ReadAll(out.Body)
}

// GetRange downloads part of an object with an HTTP Range request, so
// restoring one extent from a pack fetches only that extent.
func (s *S3) GetRange(ctx context.Context, key string, offset, length int64) ([]byte, error) {
	k, err := s.key(key)
	if err != nil {
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("s3 get %s: bad range %d+%d", key, offset, length)
	}
	if length == 0 {
		return []byte{}, nil
	}
	out, err := s.api.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    k,
		// HTTP ranges are inclusive at both ends.
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+length-1)),
	})
	if err != nil {
		switch {
		case isNotFound(err):
			return nil, ErrNotFound
		case apiCode(err) == "InvalidRange", httpStatus(err) == http.StatusRequestedRangeNotSatisfiable:
			return nil, io.ErrUnexpectedEOF
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	defer out.Body.Close()
	// S3 silently shortens a range that runs past the end of the object.
	// Read one byte more than asked for so both a short and an over-long
	// reply are caught.
	buf, err := io.ReadAll(io.LimitReader(out.Body, length+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) != length {
		return nil, io.ErrUnexpectedEOF
	}
	return buf, nil
}

// Exists asks for the object's headers without downloading it.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	k, err := s.key(key)
	if err != nil {
		return false, err
	}
	_, err = s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: k})
	if err == nil {
		return true, nil
	}
	if isNotFound(err) {
		return false, nil
	}
	return false, fmt.Errorf("s3 head %s: %w", key, err)
}

// List returns every key under prefix. S3 returns at most 1,000 keys per
// request; the paginator follows the continuation tokens for us.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	p := s3.NewListObjectsV2Paginator(s.api, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(s.prefix + prefix),
	})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list %s: %w", prefix, err)
		}
		for _, o := range page.Contents {
			k := strings.TrimPrefix(aws.ToString(o.Key), s.prefix)
			// Skip anything that isn't a valid vmobj key (for example a
			// folder marker made by a GUI tool); the engine would refuse it.
			if CheckKey(k) == nil {
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	return keys, nil
}

// Delete removes an object. S3 does not report missing keys as errors.
func (s *S3) Delete(ctx context.Context, key string) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	_, err = s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: k})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}

// isNotFound recognises "no such object" however the service reports it:
// GetObject says NoSuchKey, HeadObject has no body and so only says 404.
//
// Go note: errors.As walks the chain of wrapped errors looking for one of
// the given type, so it finds the API error inside the SDK's wrappers.
func isNotFound(err error) bool {
	switch apiCode(err) {
	case "NoSuchKey", "NotFound":
		return true
	}
	return httpStatus(err) == http.StatusNotFound
}

// apiCode returns the S3 error code ("NoSuchKey", "AccessDenied", ...), or "".
func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

func httpStatus(err error) int {
	var re *awshttp.ResponseError
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}
	return 0
}
