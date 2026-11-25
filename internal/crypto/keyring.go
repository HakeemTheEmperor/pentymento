// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

// Keyring is a repository's private keys: the current one first, then the
// previous ones that key rotation keeps, newest first. Packs are never
// rewritten, so their trailers keep data keys wrapped to the key pair of
// the day; an index rebuilt from them needs the previous keys to read.
type Keyring []PrivateKey

// Current returns the current private key, or a zero key if there is none.
func (k Keyring) Current() PrivateKey {
	if len(k) == 0 {
		return PrivateKey{}
	}
	return k[0]
}

// Unwrap recovers a data key with whichever private key it was wrapped to,
// and reports which: 0 is the current key.
func (k Keyring) Unwrap(wrapped []byte) (Key, int, error) {
	for i, priv := range k {
		if dk, err := UnwrapKey(priv, wrapped); err == nil {
			return dk, i, nil
		}
	}
	return Key{}, -1, ErrDecrypt
}

// Rewrap re-wraps a data key to pub unless it is already wrapped to the
// current key; it reports whether it changed anything.
func (k Keyring) Rewrap(pub PublicKey, wrapped []byte) ([]byte, bool, error) {
	dk, i, err := k.Unwrap(wrapped)
	if err != nil {
		return nil, false, err
	}
	defer dk.Clear()
	if i == 0 {
		return wrapped, false, nil
	}
	out, err := WrapKey(pub, dk)
	return out, err == nil, err
}

// OpenObject reverses SealObject with whichever key opens it.
func (k Keyring) OpenObject(objectKey string, sealed []byte) ([]byte, error) {
	wrapped, body, err := splitObject(sealed)
	if err != nil {
		return nil, err
	}
	dk, _, err := k.Unwrap(wrapped)
	if err != nil {
		return nil, err
	}
	defer dk.Clear()
	return open(dk, body, aad(purposeObject, []byte(objectKey)))
}

// RewrapObject re-wraps a sealed object's data key to pub unless it is
// already wrapped to the current key; the encrypted body is copied
// unchanged. It reports whether it changed anything.
func (k Keyring) RewrapObject(pub PublicKey, sealed []byte) ([]byte, bool, error) {
	wrapped, body, err := splitObject(sealed)
	if err != nil {
		return nil, false, err
	}
	rewrapped, changed, err := k.Rewrap(pub, wrapped)
	if err != nil || !changed {
		return sealed, false, err
	}
	return objectBytes(rewrapped, body), true, nil
}
