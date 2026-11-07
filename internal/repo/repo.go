// Copyright (c) 2026 Michael Allen Wilmes
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.

// Package repo is a vmobj repository: a backend plus the layout, keys and
// sealing rules on top of it.
//
// Layout:
//
//	config.json                      plaintext settings (no secrets)
//	blocks/ab/<hash>                 one sealed base block per object
//	packs/<chain>/<id>.pack          sealed extents, back to back
//	packs/<chain>/<id>.toc           sealed table of contents for a pack
//	manifests/<vm>/<name>.manifest   sealed restore points
package repo

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/wilminator/vm-objects/internal/extent"
	"github.com/wilminator/vm-objects/internal/hashing"
	"github.com/wilminator/vm-objects/internal/keys"
	"github.com/wilminator/vm-objects/internal/manifest"
	"github.com/wilminator/vm-objects/internal/metrics"
	"github.com/wilminator/vm-objects/internal/seal"
	"github.com/wilminator/vm-objects/internal/store"
)

// ConfigFormat is the repository layout version.
const ConfigFormat = 1

const configKey = "config.json"

// ConfigKey is the repository's settings object.
const ConfigKey = configKey

// Config is stored in plaintext so tools can identify a repository without
// a key. It holds no secrets: KeyID only names the key.
type Config struct {
	Format     int           `json:"format"`
	RepoID     string        `json:"repoID"`
	KeyID      string        `json:"keyID"`
	Created    time.Time     `json:"created"`
	BlockSize  extent.Length `json:"blockSize"`
	ExtentSize extent.Length `json:"extentSize"`
	PackTarget int64         `json:"packTarget"`
	// ObjectLock means every object is written with a Governance lock.
	// It is fixed at init and covered by the MAC, so it can't be switched
	// off by editing the stored config.
	ObjectLock bool `json:"objectLock,omitempty"`
	// MAC authenticates every field above with a key derived from the
	// master key, so whoever holds the storage can't alter the settings.
	MAC string `json:"mac,omitempty"`
}

// DefaultConfig uses 4 MiB base blocks, 64 KiB extents and 128 MiB packs.
func DefaultConfig() Config {
	return Config{BlockSize: 4 << 20, ExtentSize: 64 << 10, PackTarget: 128 << 20}
}

// Limits on sizes. They keep memory use bounded (each scan worker holds one
// block) and stop a damaged or hostile config from causing harm.
const (
	minExtentSize = 4 << 10
	maxBlockSize  = 64 << 20
	minPackTarget = 1 << 20
	maxPackTarget = 1 << 30
)

var hexID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// Validate checks every field is in range. RepoID is used in local folder
// names, so it must be exactly 16 hex characters.
func (c Config) Validate() error {
	switch {
	case c.Format != ConfigFormat:
		return fmt.Errorf("repository format %d is not supported by this version", c.Format)
	case !hexID.MatchString(c.RepoID):
		return fmt.Errorf("repository ID %q is malformed", c.RepoID)
	case c.ExtentSize < minExtentSize || c.BlockSize > maxBlockSize || c.BlockSize < c.ExtentSize || c.BlockSize%c.ExtentSize != 0:
		return fmt.Errorf("block size %d and extent size %d: blocks must be at most %d bytes and a multiple of extents, which must be at least %d bytes",
			c.BlockSize, c.ExtentSize, maxBlockSize, minExtentSize)
	case c.PackTarget < minPackTarget || c.PackTarget > maxPackTarget:
		return fmt.Errorf("pack size %d must be between %d and %d bytes", c.PackTarget, minPackTarget, maxPackTarget)
	}
	return nil
}

// mac computes the config's authentication code over every field but MAC.
func (c Config) mac(key [32]byte) (string, error) {
	c.MAC = ""
	data, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key[:])
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil)), nil
}

// Repo is an open repository.
type Repo struct {
	Backend store.Backend
	Config  Config
	// Metrics, when set, records how long sealing, opening and storage
	// operations take (vmobj --metrics). Nil records nothing.
	Metrics *metrics.Recorder
	sealer  *seal.Sealer
	hasher  hashing.Hasher
}

// ErrExists is returned by Init when a repository is already there.
var ErrExists = errors.New("a repository already exists here")

// Init creates a repository in an empty backend.
func Init(ctx context.Context, b store.Backend, k keys.Set, cfg Config) (*Repo, error) {
	if ok, err := b.Exists(ctx, configKey); err != nil {
		return nil, err
	} else if ok {
		return nil, ErrExists
	}
	if cfg.ObjectLock {
		if err := CheckLocking(ctx, b); err != nil {
			return nil, err
		}
	}
	cfg.Format = ConfigFormat
	cfg.RepoID = NewID()
	cfg.KeyID = k.KeyID
	cfg.Created = time.Now().UTC()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	mac, err := cfg.mac(k.Config)
	if err != nil {
		return nil, err
	}
	cfg.MAC = mac
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := b.Put(ctx, configKey, append(data, '\n')); err != nil {
		return nil, err
	}
	return newRepo(b, k, cfg)
}

// Open reads an existing repository and checks the key matches it.
func Open(ctx context.Context, b store.Backend, k keys.Set) (*Repo, error) {
	data, err := b.Get(ctx, configKey)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("no repository here; create one with \"vmobj init\"")
	}
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("reading %s: %w", configKey, err)
	}
	if cfg.KeyID != k.KeyID {
		return nil, fmt.Errorf("this key file does not match the repository (key ID %s, repository expects %s)", k.KeyID, cfg.KeyID)
	}
	want, err := cfg.mac(k.Config)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(want), []byte(cfg.MAC)) {
		return nil, fmt.Errorf("%s has been altered or damaged (authentication failed)", configKey)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return newRepo(b, k, cfg)
}

// ErrNoLocking means Object Lock was asked for but the storage can't do it.
var ErrNoLocking = errors.New("Object Lock is not available here: the bucket must be created with Object Lock enabled, and local folders have no locks")

// CheckLocking confirms the backend can lock objects.
func CheckLocking(ctx context.Context, b store.Backend) error {
	l, ok := b.(store.Locker)
	if !ok {
		return ErrNoLocking
	}
	on, err := l.LockingEnabled(ctx)
	if err != nil {
		return err
	}
	if !on {
		return ErrNoLocking
	}
	return nil
}

func newRepo(b store.Backend, k keys.Set, cfg Config) (*Repo, error) {
	if _, ok := b.(store.Locker); cfg.ObjectLock && !ok {
		return nil, ErrNoLocking
	}
	s, err := seal.New(k.Data)
	if err != nil {
		return nil, err
	}
	return &Repo{Backend: b, Config: cfg, sealer: s, hasher: hashing.Keyed(k.ID)}, nil
}

// Hasher returns the repository's keyed hasher for content IDs.
func (r *Repo) Hasher() hashing.Hasher { return r.hasher }

// NewID returns 16 random hex characters, used for chain, pack and repo IDs.
func NewID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand failed: " + err.Error()) // only on a broken OS
	}
	return hex.EncodeToString(b[:])
}

// ---- base blocks ----

// BlockKey is where a base block lives.
func BlockKey(h extent.Hash) string {
	s := h.String()
	return "blocks/" + s[:2] + "/" + s
}

// PutBlock seals and stores one base block with the given storage options.
func (r *Repo) PutBlock(ctx context.Context, h extent.Hash, data []byte, opts store.PutOptions) (int, error) {
	sealed, err := r.seal(h[:], data)
	if err != nil {
		return 0, err
	}
	return len(sealed), r.put(ctx, BlockKey(h), sealed, opts)
}

// seal, put, get and getRange are the sealer and backend calls, timed.
func (r *Repo) seal(label, data []byte) ([]byte, error) {
	done := r.Metrics.Start(metrics.Seal)
	sealed, err := r.sealer.Seal(label, data)
	done(int64(len(data)))
	return sealed, err
}

func (r *Repo) put(ctx context.Context, key string, data []byte, opts store.PutOptions) error {
	done := r.Metrics.Start(metrics.StoreWrite)
	err := store.PutWith(ctx, r.Backend, key, data, opts)
	done(int64(len(data)))
	return err
}

func (r *Repo) get(ctx context.Context, key string) ([]byte, error) {
	done := r.Metrics.Start(metrics.StoreRead)
	data, err := r.Backend.Get(ctx, key)
	done(int64(len(data)))
	return data, err
}

func (r *Repo) getRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	done := r.Metrics.Start(metrics.StoreRead)
	data, err := r.Backend.GetRange(ctx, key, off, n)
	done(int64(len(data)))
	return data, err
}

func (r *Repo) list(ctx context.Context, prefix string) ([]string, error) {
	done := r.Metrics.Start(metrics.StoreList)
	keys, err := r.Backend.List(ctx, prefix)
	done(0)
	return keys, err
}

// GetBlock fetches a base block and checks it really has hash h.
func (r *Repo) GetBlock(ctx context.Context, h extent.Hash) ([]byte, error) {
	sealed, err := r.get(ctx, BlockKey(h))
	if err != nil {
		return nil, fmt.Errorf("block %s: %w", h, err)
	}
	return r.openContent(h, sealed)
}

// BlockSet lists every stored base block. One listing is much cheaper than
// asking about each block separately, especially on S3.
func (r *Repo) BlockSet(ctx context.Context) (map[extent.Hash]bool, error) {
	keys, err := r.list(ctx, "blocks/")
	if err != nil {
		return nil, err
	}
	set := make(map[extent.Hash]bool, len(keys))
	for _, k := range keys {
		h, err := extent.ParseHash(path.Base(k))
		if err != nil {
			continue // not ours; ignore
		}
		set[h] = true
	}
	return set, nil
}

func (r *Repo) openContent(h extent.Hash, sealed []byte) ([]byte, error) {
	done := r.Metrics.Start(metrics.Open)
	defer func() { done(int64(len(sealed))) }()
	data, err := r.sealer.Open(h[:], sealed)
	if err != nil {
		return nil, fmt.Errorf("object %s: %w", h, err)
	}
	if r.hasher.Sum(data) != h {
		return nil, fmt.Errorf("object %s: contents do not match their hash", h)
	}
	return data, nil
}

// ---- manifests ----

// ManifestKey is where a manifest lives.
func ManifestKey(vm, name string) string { return "manifests/" + vm + "/" + name + ".manifest" }

// PutManifest seals and stores a manifest. It is written last in every
// run, so a manifest only exists once everything it points at is stored.
func (r *Repo) PutManifest(ctx context.Context, m *manifest.Manifest, opts store.PutOptions) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	key := ManifestKey(m.VM, m.Name)
	sealed, err := r.sealer.Seal([]byte(key), data)
	if err != nil {
		return err
	}
	return store.PutWith(ctx, r.Backend, key, sealed, opts)
}

// GetManifest reads and checks a manifest.
func (r *Repo) GetManifest(ctx context.Context, vm, name string) (*manifest.Manifest, error) {
	key := ManifestKey(vm, name)
	sealed, err := r.Backend.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("manifest %s/%s: %w", vm, name, err)
	}
	data, err := r.sealer.Open([]byte(key), sealed)
	if err != nil {
		return nil, fmt.Errorf("manifest %s/%s: %w", vm, name, err)
	}
	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s/%s: %w", vm, name, err)
	}
	if m.VM != vm || m.Name != name {
		return nil, fmt.Errorf("manifest %s/%s: names inside do not match", vm, name)
	}
	return &m, m.Validate()
}

// ListManifests returns a VM's manifest names, oldest first.
func (r *Repo) ListManifests(ctx context.Context, vm string) ([]string, error) {
	prefix := "manifests/" + vm + "/"
	keys, err := r.Backend.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, k := range keys {
		if strings.HasSuffix(k, ".manifest") {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(k, prefix), ".manifest"))
		}
	}
	// Storage lists keys in plain text order, which puts "…-incremental-2"
	// before "…-incremental" (the ".manifest" ending sorts after "-").
	slices.SortStableFunc(names, manifest.CompareNames)
	return names, nil
}

// ListVMs returns the VMs that have manifests.
func (r *Repo) ListVMs(ctx context.Context) ([]string, error) {
	keys, err := r.Backend.List(ctx, "manifests/")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var vms []string
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) == 3 && !seen[parts[1]] {
			seen[parts[1]] = true
			vms = append(vms, parts[1])
		}
	}
	return vms, nil
}

// GetSealedBlock returns a block's stored (encrypted) bytes after checking
// they decrypt to content with hash h. Copying between repositories that
// share a key moves these bytes as they are.
func (r *Repo) GetSealedBlock(ctx context.Context, h extent.Hash) ([]byte, error) {
	sealed, err := r.get(ctx, BlockKey(h))
	if err != nil {
		return nil, fmt.Errorf("block %s: %w", h, err)
	}
	if _, err := r.openContent(h, sealed); err != nil {
		return nil, err
	}
	return sealed, nil
}

// PutSealedBlock stores bytes from GetSealedBlock of a repository with the
// same key. The caller must have checked the keys match.
func (r *Repo) PutSealedBlock(ctx context.Context, h extent.Hash, sealed []byte, opts store.PutOptions) error {
	return r.put(ctx, BlockKey(h), sealed, opts)
}
