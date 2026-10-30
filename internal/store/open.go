// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Location is a parsed s3:// repository address.
//
//	s3://BUCKET[/PREFIX][?region=R][&endpoint=URL][&profile=P][&path-style=true|false]
//
// Credentials are deliberately not part of the address. They come from the
// standard AWS sources: the AWS_* environment variables, ~/.aws/credentials
// and ~/.aws/config (choose a section with profile=), or an instance role.
type S3Location struct {
	Bucket    string
	Prefix    string
	Region    string
	Endpoint  string // empty for AWS itself; set for QuObjects, MinIO and the like
	Profile   string
	PathStyle bool // bucket in the path, not the host name; needed by most non-AWS services
}

// ParseS3 parses an s3:// address.
func ParseS3(raw string) (S3Location, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return S3Location{}, err
	}
	if u.Scheme != "s3" || u.Host == "" {
		return S3Location{}, fmt.Errorf("%q: want s3://BUCKET[/PREFIX]", raw)
	}
	if u.User != nil {
		return S3Location{}, fmt.Errorf("%q: put credentials in ~/.aws/credentials or the environment, not in the address", raw)
	}
	q := u.Query()
	loc := S3Location{
		Bucket:   u.Host,
		Prefix:   strings.Trim(u.Path, "/"),
		Region:   q.Get("region"),
		Endpoint: q.Get("endpoint"),
		Profile:  q.Get("profile"),
	}
	// Path style defaults on whenever a custom endpoint is given, since
	// that is what self-hosted services expect.
	loc.PathStyle = loc.Endpoint != ""
	switch q.Get("path-style") {
	case "":
	case "true":
		loc.PathStyle = true
	case "false":
		loc.PathStyle = false
	default:
		return S3Location{}, fmt.Errorf("%q: path-style must be true or false", raw)
	}
	for k := range q {
		switch k {
		case "region", "endpoint", "profile", "path-style":
		default:
			return S3Location{}, fmt.Errorf("%q: unknown option %q", raw, k)
		}
	}
	if loc.Endpoint != "" {
		e, err := url.Parse(loc.Endpoint)
		if err != nil || (e.Scheme != "https" && e.Scheme != "http") || e.Host == "" {
			return S3Location{}, fmt.Errorf("%q: endpoint must be an http:// or https:// URL", raw)
		}
	}
	return loc, nil
}

// Open returns the backend for a repository location: an s3:// address,
// or anything else as a local folder path.
func Open(ctx context.Context, location string) (Backend, error) {
	if !strings.HasPrefix(location, "s3://") {
		return NewLocal(location)
	}
	loc, err := ParseS3(location)
	if err != nil {
		return nil, err
	}
	client, err := NewS3Client(ctx, loc)
	if err != nil {
		return nil, err
	}
	return NewS3(client, loc.Bucket, loc.Prefix)
}

// NewS3Client builds an AWS SDK client for loc.
func NewS3Client(ctx context.Context, loc S3Location) (*s3.Client, error) {
	var opts []func(*config.LoadOptions) error
	if loc.Region != "" {
		opts = append(opts, config.WithRegion(loc.Region))
	}
	if loc.Profile != "" {
		opts = append(opts, config.WithSharedConfigProfile(loc.Profile))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS settings: %w", err)
	}
	if cfg.Region == "" {
		if loc.Endpoint == "" {
			return nil, fmt.Errorf("no AWS region: add ?region=... to the s3:// address or set AWS_REGION")
		}
		// Self-hosted services ignore the region but the request signer needs one.
		cfg.Region = "us-east-1"
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if loc.Endpoint != "" {
			o.BaseEndpoint = aws.String(loc.Endpoint)
			// Newer SDKs add CRC checksums to every upload by default,
			// which several S3-compatible services reject. Puts already
			// carry Content-MD5, so ask for the extra checksums only when
			// an operation requires them.
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
			o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		}
		o.UsePathStyle = loc.PathStyle
	}), nil
}
