// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Object Lock on S3.
//
// Every lock vmobj sets is Governance mode. Nothing in vmobj ever sends the
// x-amz-bypass-governance-retention header, so no vmobj command (or a bug
// in one) can delete a locked version or shorten its lock. Recovering from
// a mistake means an administrator using an account that holds the
// s3:BypassGovernanceRetention permission, which vmobj's own credentials
// should never have.

// LockingEnabled asks the bucket whether Object Lock is turned on.
func (s *S3) LockingEnabled(ctx context.Context) (bool, error) {
	out, err := s.api.GetObjectLockConfiguration(ctx, &s3.GetObjectLockConfigurationInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		if apiCode(err) == "ObjectLockConfigurationNotFoundError" {
			return false, nil
		}
		return false, fmt.Errorf("s3 object lock configuration: %w", err)
	}
	return out.ObjectLockConfiguration != nil &&
		out.ObjectLockConfiguration.ObjectLockEnabled == types.ObjectLockEnabledEnabled, nil
}

// ExtendLock moves the current version's lock to until. S3 itself refuses
// to shorten a Governance lock without the bypass header, so a mistaken
// earlier date fails rather than weakening protection.
func (s *S3) ExtendLock(ctx context.Context, key string, until time.Time) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	_, err = s.api.PutObjectRetention(ctx, &s3.PutObjectRetentionInput{
		Bucket: aws.String(s.bucket),
		Key:    k,
		Retention: &types.ObjectLockRetention{
			Mode:            types.ObjectLockRetentionModeGovernance,
			RetainUntilDate: aws.Time(until.UTC()),
		},
	})
	if err != nil {
		if isLockError(err) {
			return fmt.Errorf("s3 extend lock %s: %w (%v)", key, ErrLocked, err)
		}
		return fmt.Errorf("s3 extend lock %s: %w", key, err)
	}
	return nil
}

// ListVersions lists every version and delete marker under prefix.
func (s *S3) ListVersions(ctx context.Context, prefix string) ([]Version, error) {
	var out []Version
	in := &s3.ListObjectVersionsInput{Bucket: aws.String(s.bucket), Prefix: aws.String(s.prefix + prefix)}
	for {
		page, err := s.api.ListObjectVersions(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("s3 list versions %s: %w", prefix, err)
		}
		for _, v := range page.Versions {
			out = append(out, Version{
				Key: strings.TrimPrefix(aws.ToString(v.Key), s.prefix), VersionID: aws.ToString(v.VersionId),
				Latest: aws.ToBool(v.IsLatest), Modified: aws.ToTime(v.LastModified), Size: aws.ToInt64(v.Size),
			})
		}
		for _, m := range page.DeleteMarkers {
			out = append(out, Version{
				Key: strings.TrimPrefix(aws.ToString(m.Key), s.prefix), VersionID: aws.ToString(m.VersionId),
				Latest: aws.ToBool(m.IsLatest), Modified: aws.ToTime(m.LastModified), DeleteMarker: true,
			})
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		in.KeyMarker, in.VersionIdMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	// Keep only well-formed vmobj keys, as List does.
	kept := out[:0]
	for _, v := range out {
		if CheckKey(v.Key) == nil {
			kept = append(kept, v)
		}
	}
	return kept, nil
}

// StatVersion reads one version's headers, including its lock.
func (s *S3) StatVersion(ctx context.Context, key, versionID string) (ObjectInfo, error) {
	k, err := s.key(key)
	if err != nil {
		return ObjectInfo{}, err
	}
	out, err := s.api.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: k, VersionId: aws.String(versionID)})
	if err != nil {
		if isNotFound(err) {
			return ObjectInfo{}, ErrNotFound
		}
		return ObjectInfo{}, fmt.Errorf("s3 head %s@%s: %w", key, versionID, err)
	}
	return headInfo(out), nil
}

// DeleteVersion permanently removes one version. Locked versions are
// refused by S3 with AccessDenied, reported as ErrLocked.
func (s *S3) DeleteVersion(ctx context.Context, key, versionID string) error {
	k, err := s.key(key)
	if err != nil {
		return err
	}
	if versionID == "" {
		return fmt.Errorf("s3 delete %s: no version given", key)
	}
	_, err = s.api.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: k, VersionId: aws.String(versionID)})
	if err != nil {
		if isLockError(err) {
			return fmt.Errorf("s3 delete %s@%s: %w (%v)", key, versionID, ErrLocked, err)
		}
		if !isNotFound(err) {
			return fmt.Errorf("s3 delete %s@%s: %w", key, versionID, err)
		}
	}
	return nil
}

// headInfo turns HeadObject's headers into an ObjectInfo.
func headInfo(out *s3.HeadObjectOutput) ObjectInfo {
	info := ObjectInfo{
		Size:      aws.ToInt64(out.ContentLength),
		Modified:  aws.ToTime(out.LastModified),
		Class:     string(out.StorageClass),
		VersionID: aws.ToString(out.VersionId),
		LockUntil: aws.ToTime(out.ObjectLockRetainUntilDate),
	}
	switch out.StorageClass {
	case types.StorageClassGlacier, types.StorageClassDeepArchive:
		info.Archived = true
	case types.StorageClassIntelligentTiering:
		// Only Intelligent-Tiering's opt-in archive tiers need a restore.
		info.Archived = out.ArchiveStatus != ""
	}
	info.Restoring, info.RestoredUntil = parseRestore(aws.ToString(out.Restore))
	return info
}

// isLockError recognises "refused because of Object Lock". AWS answers
// AccessDenied; other services use their own codes (MinIO, for one, can
// answer InvalidRequest with a "WORM protected" message), so the message
// is checked too.
func isLockError(err error) bool {
	switch apiCode(err) {
	case "AccessDenied", "ObjectLocked":
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		m := strings.ToLower(ae.ErrorMessage())
		return strings.Contains(m, "worm") || strings.Contains(m, "object lock") || strings.Contains(m, "retention")
	}
	return false
}
