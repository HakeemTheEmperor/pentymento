// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"filippo.io/age"
	"golang.org/x/crypto/chacha20poly1305"
)

// ErrDecrypt means a ciphertext did not authenticate: wrong key, tampered
// data, or data moved to another object.
var ErrDecrypt = errors.New("decryption failed: wrong key or tampered data")

// Overhead is what XChaCha20-Poly1305 adds to each sealed message: a
// 24-byte nonce and a 16-byte tag.
const Overhead = chacha20poly1305.NonceSizeX + chacha20poly1305.Overhead

// Purposes, used as the start of the associated data so a ciphertext made
// for one purpose never opens as another.
const (
	purposeChunk    = "pentymento chunk v1"
	purposeMetadata = "pentymento metadata v1"
	purposeObject   = "pentymento object v1"
	purposePrivate  = "pentymento private key v1"
	purposePublic   = "pentymento public key v1"
	purposeSlot     = "pentymento slot v1"
)

func aad(purpose string, id []byte) []byte {
	return append(append([]byte(purpose), 0), id...)
}

// seal encrypts plaintext with XChaCha20-Poly1305 under key, with a random
// nonce, and returns nonce || ciphertext.
func seal(key Key, plaintext, ad []byte) []byte {
	a, err := chacha20poly1305.NewX(key.Bytes())
	if err != nil {
		panic(err) // unreachable: keys are always 32 bytes
	}
	out := make([]byte, chacha20poly1305.NonceSizeX, chacha20poly1305.NonceSizeX+len(plaintext)+chacha20poly1305.Overhead)
	if _, err := rand.Read(out); err != nil {
		panic(err)
	}
	return a.Seal(out, out, plaintext, ad)
}

// open reverses seal.
func open(key Key, sealed, ad []byte) ([]byte, error) {
	if key.IsZero() {
		return nil, ErrDecrypt
	}
	a, err := chacha20poly1305.NewX(key.Bytes())
	if err != nil {
		return nil, err
	}
	if len(sealed) < Overhead {
		return nil, ErrDecrypt
	}
	n := chacha20poly1305.NonceSizeX
	out, err := a.Open(nil, sealed[:n], sealed[n:], ad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return out, nil
}

// EncryptChunk encrypts one chunk (its compression header and data) under
// the pack's data key, bound to the chunk's ID.
func EncryptChunk(dataKey Key, id [32]byte, plaintext []byte) []byte {
	return seal(dataKey, plaintext, aad(purposeChunk, id[:]))
}

// DecryptChunk reverses EncryptChunk; it fails if the chunk was encrypted
// for another ID.
func DecryptChunk(dataKey Key, id [32]byte, sealed []byte) ([]byte, error) {
	return open(dataKey, sealed, aad(purposeChunk, id[:]))
}

// EncryptMetadata encrypts a metadata object (snapshot, index, event,
// checkpoint, heartbeat, journal entry) with the metadata key, bound to
// the object's key in the repository.
func EncryptMetadata(metadataKey Key, objectKey string, plaintext []byte) []byte {
	return seal(metadataKey, plaintext, aad(purposeMetadata, []byte(objectKey)))
}

// DecryptMetadata reverses EncryptMetadata.
func DecryptMetadata(metadataKey Key, objectKey string, sealed []byte) ([]byte, error) {
	return open(metadataKey, sealed, aad(purposeMetadata, []byte(objectKey)))
}

// WrapKey encrypts a data key to the repository's public key with age
// (X25519, HKDF-SHA-256, ChaCha20-Poly1305).
func WrapKey(pub PublicKey, dataKey Key) ([]byte, error) {
	if pub.IsZero() {
		return nil, errors.New("no public key")
	}
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, pub.r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(dataKey.Bytes()); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnwrapKey recovers a data key with the repository's private key.
func UnwrapKey(priv PrivateKey, wrapped []byte) (Key, error) {
	if priv.IsZero() {
		return Key{}, ErrDecrypt
	}
	r, err := age.Decrypt(bytes.NewReader(wrapped), priv.identity())
	if err != nil {
		return Key{}, ErrDecrypt
	}
	b, err := io.ReadAll(io.LimitReader(r, KeySize+1))
	if err != nil || len(b) != KeySize {
		return Key{}, ErrDecrypt
	}
	k, _ := KeyFromBytes(b)
	clear(b)
	return k, nil
}

// RewrapKey re-encrypts a wrapped data key to a new public key, for key
// rotation. The data it protects is not touched.
func RewrapKey(old PrivateKey, pub PublicKey, wrapped []byte) ([]byte, error) {
	k, err := UnwrapKey(old, wrapped)
	if err != nil {
		return nil, err
	}
	defer k.Clear()
	return WrapKey(pub, k)
}

// Sealed objects (log segments, timeline history files) carry their own
// wrapped data key in a header:
//
//	"PTMo" | version 1 | wrapped-key length (uint16, big-endian) | wrapped key | nonce | ciphertext
var objectMagic = []byte("PTMo")

const objectVersion = 1

// SealObject encrypts a whole object (one log segment) under a fresh data
// key wrapped to the public key, bound to the object's key.
func SealObject(pub PublicKey, objectKey string, plaintext []byte) ([]byte, error) {
	dk := NewKey()
	defer dk.Clear()
	wrapped, err := WrapKey(pub, dk)
	if err != nil {
		return nil, err
	}
	return objectBytes(wrapped, seal(dk, plaintext, aad(purposeObject, []byte(objectKey)))), nil
}

func objectBytes(wrapped, body []byte) []byte {
	out := make([]byte, 0, len(objectMagic)+3+len(wrapped)+len(body))
	out = append(out, objectMagic...)
	out = append(out, objectVersion)
	out = binary.BigEndian.AppendUint16(out, uint16(len(wrapped))) // #nosec G115 -- an age header is a few hundred bytes
	out = append(out, wrapped...)
	return append(out, body...)
}

// splitObject returns a sealed object's wrapped key and encrypted body.
func splitObject(sealed []byte) (wrapped, body []byte, err error) {
	h := len(objectMagic) + 3
	if len(sealed) < h || !bytes.Equal(sealed[:len(objectMagic)], objectMagic) {
		return nil, nil, fmt.Errorf("%w: not a sealed object", ErrDecrypt)
	}
	if v := sealed[len(objectMagic)]; v != objectVersion {
		return nil, nil, fmt.Errorf("sealed object version %d is newer than this pentym understands", v)
	}
	n := int(binary.BigEndian.Uint16(sealed[len(objectMagic)+1:]))
	if len(sealed) < h+n {
		return nil, nil, fmt.Errorf("%w: truncated header", ErrDecrypt)
	}
	return sealed[h : h+n], sealed[h+n:], nil
}

// OpenObject reverses SealObject.
func OpenObject(priv PrivateKey, objectKey string, sealed []byte) ([]byte, error) {
	wrapped, body, err := splitObject(sealed)
	if err != nil {
		return nil, err
	}
	dk, err := UnwrapKey(priv, wrapped)
	if err != nil {
		return nil, err
	}
	defer dk.Clear()
	return open(dk, body, aad(purposeObject, []byte(objectKey)))
}

// RewrapObject re-wraps a sealed object's data key to a new public key,
// for key rotation. The encrypted body is copied unchanged.
func RewrapObject(old PrivateKey, pub PublicKey, sealed []byte) ([]byte, error) {
	wrapped, body, err := splitObject(sealed)
	if err != nil {
		return nil, err
	}
	rewrapped, err := RewrapKey(old, pub, wrapped)
	if err != nil {
		return nil, err
	}
	return objectBytes(rewrapped, body), nil
}
