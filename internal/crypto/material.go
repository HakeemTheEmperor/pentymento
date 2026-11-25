// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"errors"
	"fmt"
)

// Material is a repository's key material, as a full slot unlocks it.
//
//   - Master protects the private key (the keys/private object); full
//     slots protect Master.
//   - Metadata encrypts metadata and authenticates the public key (the
//     keys/public object).
//   - ChunkID keys the chunk-ID hash and the chunker's gear table.
//   - Private is the age X25519 identity that unwraps data keys.
type Material struct {
	Master   Key
	Metadata Key
	ChunkID  Key
	Private  PrivateKey
}

// Generate creates new key material for a repository.
func Generate() (*Material, error) {
	priv, err := NewPrivateKey()
	if err != nil {
		return nil, err
	}
	return &Material{Master: NewKey(), Metadata: NewKey(), ChunkID: NewKey(), Private: priv}, nil
}

// Public returns the repository's public key.
func (m *Material) Public() PublicKey { return m.Private.Public() }

// Unlocked returns what a full slot stores.
func (m *Material) Unlocked() Unlocked {
	return Unlocked{Scope: ScopeFull, Master: m.Master, Metadata: m.Metadata, ChunkID: m.ChunkID}
}

// SealPrivate encrypts the private key with the master key, for the
// keys/private object.
func SealPrivate(master Key, priv PrivateKey) []byte {
	s := []byte(priv.identity().String())
	defer clear(s)
	return seal(master, s, aad(purposePrivate, nil))
}

// OpenPrivate decrypts the keys/private object.
func OpenPrivate(master Key, sealed []byte) (PrivateKey, error) {
	b, err := open(master, sealed, aad(purposePrivate, nil))
	if err != nil {
		return PrivateKey{}, err
	}
	defer clear(b)
	return parsePrivateKey(string(b))
}

// SealPublic encrypts and authenticates the public key with the metadata
// key, for the keys/public object, so that whoever holds the metadata key
// (agents included) finds the current public key and a bucket writer
// without the key cannot substitute their own.
func SealPublic(metadata Key, pub PublicKey) []byte {
	return seal(metadata, []byte(pub.String()), aad(purposePublic, nil))
}

// OpenPublic decrypts the keys/public object.
func OpenPublic(metadata Key, sealed []byte) (PublicKey, error) {
	b, err := open(metadata, sealed, aad(purposePublic, nil))
	if err != nil {
		return PublicKey{}, err
	}
	return ParsePublicKey(string(b))
}

// Rotation is the result of Rotate.
type Rotation struct {
	// Private is the new private key; Material.Private still holds the
	// old one, needed to re-wrap existing data keys.
	Private PrivateKey
	// PrivateObject and PublicObject replace keys/private and keys/public.
	PrivateObject []byte
	PublicObject  []byte
}

// Rotate generates a new key pair (key rotate). The caller re-wraps every
// data key with RewrapKey and RewrapObject, writes the new keys/private and
// keys/public objects, and only then discards the old private key. Slots
// are unchanged: they protect the master key, not the key pair.
func Rotate(m *Material) (*Rotation, error) {
	if m.Master.IsZero() || m.Metadata.IsZero() {
		return nil, errors.New("rotation needs the master and metadata keys from a full slot")
	}
	priv, err := NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("generate key pair: %w", err)
	}
	return &Rotation{
		Private:       priv,
		PrivateObject: SealPrivate(m.Master, priv),
		PublicObject:  SealPublic(m.Metadata, priv.Public()),
	}, nil
}
