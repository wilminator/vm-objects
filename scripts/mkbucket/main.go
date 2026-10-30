// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Command mkbucket creates a test bucket with Object Lock enabled, so tests
// against MinIO or another local S3 service need no AWS CLI.
//
//	go run ./scripts/mkbucket 's3://BUCKET?endpoint=http://127.0.0.1:9000[&profile=P]'
//
// Credentials come from the usual AWS environment variables or files. An
// existing bucket that this account owns is not an error.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/wilminator/vm-objects/internal/store"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkbucket s3://BUCKET?endpoint=URL")
		os.Exit(2)
	}
	ctx := context.Background()
	loc, err := store.ParseS3(os.Args[1])
	if err != nil {
		fail(err)
	}
	client, err := store.NewS3Client(ctx, loc)
	if err != nil {
		fail(err)
	}
	_, err = client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket:                     aws.String(loc.Bucket),
		ObjectLockEnabledForBucket: aws.Bool(true),
	})
	var ae smithy.APIError
	if err != nil && !(errors.As(err, &ae) && ae.ErrorCode() == "BucketAlreadyOwnedByYou") {
		fail(err)
	}
	fmt.Printf("bucket %s is ready (Object Lock enabled)\n", loc.Bucket)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkbucket:", err)
	os.Exit(1)
}
