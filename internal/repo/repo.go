// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package repo

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/zeebo/blake3"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/crypto"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// maxMetadataSize bounds a metadata object read into memory.
const maxMetadataSize = 64 << 20

// ErrLocked means an operation needs keys the repository has not been
// unlocked with.
var ErrLocked = errors.New("repository is locked: unlock it with a key slot first")

// ErrNoPrivateKey means an operation needs a full slot.
var ErrNoPrivateKey = errors.New("this needs a full key slot; an agent slot can write but not read data")

// Repository is an open repository. Open it with Open, then Unlock it with
// a key slot (except in insecure test mode).
type Repository struct {
	be  backend.Backend
	cfg Config

	unlocked bool
	scope    crypto.Scope
	metadata crypto.Key
	chunkID  crypto.Key
	public   crypto.PublicKey
	private  crypto.PrivateKey // zero for an agent unlock

	cache *Cache // nil: no local cache

	// Now is the clock; tests replace it.
	Now func() time.Time
}

// Create initializes a repository on an empty backend: the config, the key
// objects and the slots. In insecure test mode m and slots are nil.
func Create(ctx context.Context, be backend.Backend, cfg Config, m *crypto.Material, slots []*crypto.Slot) (*Repository, error) {
	cb, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	if !cfg.Insecure() {
		if m == nil || len(slots) == 0 {
			return nil, errors.New("an encrypted repository needs key material and at least one slot")
		}
		if err := be.Put(ctx, privateKeyObject, bytes.NewReader(crypto.SealPrivate(m.Master, m.Private)), backend.PutOptions{IfNotExists: true}); err != nil {
			return nil, existsOr(err)
		}
		if err := be.Put(ctx, publicKeyObject, bytes.NewReader(crypto.SealPublic(m.Metadata, m.Public())), backend.PutOptions{IfNotExists: true}); err != nil {
			return nil, existsOr(err)
		}
		for _, s := range slots {
			if err := putSlot(ctx, be, s); err != nil {
				return nil, err
			}
		}
	}
	// The config is written last: a repository without one is not a
	// repository, so an interrupted create can simply be retried.
	if err := be.Put(ctx, configKey, bytes.NewReader(cb), backend.PutOptions{IfNotExists: true}); err != nil {
		return nil, existsOr(err)
	}
	r := &Repository{be: be, cfg: cfg}
	if cfg.Insecure() {
		r.unlocked, r.scope = true, crypto.ScopeFull
	} else if err := r.Unlock(ctx, m.Unlocked()); err != nil {
		return nil, err
	}
	return r, nil
}

func existsOr(err error) error {
	if errors.Is(err, backend.ErrExists) {
		return &msg.Error{Code: msg.ExitFailure, Err: err, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDExists,
			Fact: "a repository already exists at this location",
			Next: "use the existing repository, or choose an empty location",
		}}
	}
	return err
}

// Open reads the repository's config and checks its format version.
func Open(ctx context.Context, be backend.Backend) (*Repository, error) {
	b, err := readAll(ctx, be, configKey, 1<<20)
	if errors.Is(err, backend.ErrNotFound) {
		return nil, &msg.Error{Code: msg.ExitNotFound, Err: err, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDNotFound,
			Fact: "no repository found at this location (no config object)",
			Next: "check the repository location in the config, or create one with pentym init",
		}}
	}
	if err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(b)
	if err != nil {
		return nil, err
	}
	r := &Repository{be: be, cfg: cfg}
	if cfg.Insecure() {
		r.unlocked, r.scope = true, crypto.ScopeFull
	}
	return r, nil
}

// Config returns the repository's config.
func (r *Repository) Config() Config { return r.cfg }

// Backend returns the storage backend.
func (r *Repository) Backend() backend.Backend { return r.be }

// Unlock loads the keys a slot unlocked: the metadata and chunk-ID keys,
// the current public key and, for a full slot, the private key.
func (r *Repository) Unlock(ctx context.Context, u crypto.Unlocked) error {
	if r.cfg.Insecure() {
		return nil
	}
	pb, err := readAll(ctx, r.be, publicKeyObject, 4096)
	if err != nil {
		return fmt.Errorf("read the public key: %w", err)
	}
	pub, err := crypto.OpenPublic(u.Metadata, pb)
	if err != nil {
		return fmt.Errorf("the slot's keys do not match this repository: %w", err)
	}
	var priv crypto.PrivateKey
	if u.Scope == crypto.ScopeFull {
		kb, err := readAll(ctx, r.be, privateKeyObject, 4096)
		if err != nil {
			return fmt.Errorf("read the private key: %w", err)
		}
		if priv, err = crypto.OpenPrivate(u.Master, kb); err != nil {
			return fmt.Errorf("open the private key: %w", err)
		}
	}
	r.unlocked, r.scope = true, u.Scope
	r.metadata, r.chunkID, r.public, r.private = u.Metadata, u.ChunkID, pub, priv
	return nil
}

// CanRead reports whether the repository was unlocked with a full slot (or
// is unencrypted), so data can be decrypted.
func (r *Repository) CanRead() bool { return r.unlocked && r.scope == crypto.ScopeFull }

// PublicKey returns the current public key.
func (r *Repository) PublicKey() crypto.PublicKey { return r.public }

// ChunkIDKey returns the chunk-ID key.
func (r *Repository) ChunkIDKey() crypto.Key { return r.chunkID }

func (r *Repository) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Slots returns the repository's key slots, which can be read before
// unlocking.
func (r *Repository) Slots(ctx context.Context) ([]*crypto.Slot, error) {
	return Slots(ctx, r.be)
}

// Slots reads the key slots from a backend.
func Slots(ctx context.Context, be backend.Backend) ([]*crypto.Slot, error) {
	var keys []string
	if err := be.List(ctx, slotPrefix, func(oi backend.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	}); err != nil {
		return nil, err
	}
	var slots []*crypto.Slot
	for _, k := range keys {
		b, err := readAll(ctx, be, k, 64<<10)
		if err != nil {
			return nil, err
		}
		s, err := crypto.ParseSlot(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		if slotKey(s.ID) != k {
			return nil, fmt.Errorf("%s holds slot %s", k, s.ID)
		}
		slots = append(slots, s)
	}
	return slots, nil
}

// AddSlot stores a new key slot.
func (r *Repository) AddSlot(ctx context.Context, s *crypto.Slot) error { return putSlot(ctx, r.be, s) }

func putSlot(ctx context.Context, be backend.Backend, s *crypto.Slot) error {
	b, err := s.Marshal()
	if err != nil {
		return err
	}
	return be.Put(ctx, slotKey(s.ID), bytes.NewReader(b), backend.PutOptions{IfNotExists: true})
}

// RemoveSlot deletes a key slot, refusing to remove the last full slot.
func (r *Repository) RemoveSlot(ctx context.Context, id string) error {
	slots, err := r.Slots(ctx)
	if err != nil {
		return err
	}
	if err := crypto.CheckRemove(slots, id); err != nil {
		return err
	}
	return r.be.Delete(ctx, slotKey(id))
}

// targetIDContext separates target IDs from other uses of the metadata key.
const targetIDContext = "pentymento target id v1"

// TargetID returns the opaque ID used in paths for a target name: keyed
// BLAKE3 of the name under the metadata key, 16 hex characters. The same
// name always gives the same ID in a repository; without the metadata key
// the ID reveals nothing about the name.
func (r *Repository) TargetID(name string) (string, error) {
	var h *blake3.Hasher
	if r.cfg.Insecure() {
		h = blake3.New()
	} else {
		if !r.unlocked {
			return "", ErrLocked
		}
		var err error
		if h, err = blake3.NewKeyed(r.metadata.Bytes()); err != nil {
			return "", err
		}
	}
	_, _ = h.WriteString(targetIDContext + "\x00" + name)
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// putMeta encodes v as JSON, encrypts it with the metadata key bound to
// key, and writes it.
func (r *Repository) putMeta(ctx context.Context, key string, v any, opts backend.PutOptions) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if !r.cfg.Insecure() {
		if !r.unlocked {
			return ErrLocked
		}
		b = crypto.EncryptMetadata(r.metadata, key, b)
	}
	return r.be.Put(ctx, key, bytes.NewReader(b), opts)
}

// getMeta reads and decrypts a metadata object into v.
func (r *Repository) getMeta(ctx context.Context, key string, v any) error {
	b, err := readAll(ctx, r.be, key, maxMetadataSize)
	if err != nil {
		return err
	}
	return r.decodeMeta(key, b, v)
}

// getMetaCached is getMeta for immutable objects, through the local cache.
// A cached copy is used only if it decrypts.
func (r *Repository) getMetaCached(ctx context.Context, key string, v any) error {
	if !r.cfg.Insecure() && !r.unlocked {
		return ErrLocked
	}
	b, err := r.cachedRead(ctx, key, maxMetadataSize, func(b []byte) bool {
		if r.cfg.Insecure() {
			return json.Valid(b)
		}
		_, err := crypto.DecryptMetadata(r.metadata, key, b)
		return err == nil
	})
	if err != nil {
		return err
	}
	return r.decodeMeta(key, b, v)
}

func (r *Repository) decodeMeta(key string, b []byte, v any) error {
	var err error
	if !r.cfg.Insecure() {
		if !r.unlocked {
			return ErrLocked
		}
		if b, err = crypto.DecryptMetadata(r.metadata, key, b); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s is malformed: %w", key, err)
	}
	return nil
}

// listKeys returns the keys under prefix in order.
func (r *Repository) listKeys(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := r.be.List(ctx, prefix, func(oi backend.ObjectInfo) error {
		keys = append(keys, oi.Key)
		return nil
	})
	sort.Strings(keys)
	return keys, err
}

// readAll reads a whole object of at most limit bytes.
func readAll(ctx context.Context, be backend.Backend, key string, limit int64) ([]byte, error) {
	rc, err := be.Get(ctx, key, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s is larger than %d bytes", key, limit)
	}
	return b, nil
}

// lastKey returns the last key under prefix, or "" when there is none.
func (r *Repository) lastKey(ctx context.Context, prefix string) (string, error) {
	keys, err := r.listKeys(ctx, prefix)
	if err != nil || len(keys) == 0 {
		return "", err
	}
	return keys[len(keys)-1], nil
}
