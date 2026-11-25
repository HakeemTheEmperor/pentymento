# Cryptographic design

This document specifies how Pentymento encrypts a repository, for public review. The design goal: **a compromised database host can neither read nor delete backup history.** The agent that writes backups holds no key that decrypts them.

Pentymento implements no cryptographic primitive. It uses:

| Primitive | Library | Used for |
| --- | --- | --- |
| XChaCha20-Poly1305 | `golang.org/x/crypto/chacha20poly1305` | Chunks, metadata, slots, key objects |
| X25519 + HKDF-SHA-256 + ChaCha20-Poly1305 (age v1) | `filippo.io/age` | Wrapping data keys to the repository's public key; age-identity slots |
| Argon2id | `golang.org/x/crypto/argon2` | Passphrase and paper-key slots |
| BLAKE3 (keyed and key-derivation modes) | `github.com/zeebo/blake3` | Chunk IDs, chunker gear table |
| CSPRNG | `crypto/rand` | Every key, nonce and salt |

A test fails the build if any other cryptographic package is imported.

## Keys

| Key | Size | Held in | Used for |
| --- | --- | --- | --- |
| Master key | 256 bits | Full slots | Encrypting the private key (`keys/private`) |
| Repository key pair | age X25519 | Private: `keys/private`. Public: `keys/public` | Wrapping per-pack and per-segment data keys |
| Metadata key | 256 bits | Full slots and agent slots | Snapshots, index, events, checkpoints, heartbeats, journal; authenticating `keys/public` |
| Chunk-ID key | 256 bits | Full slots and agent slots | Keyed BLAKE3 chunk IDs; the chunker's gear table |
| Data key | 256 bits | Never stored unwrapped | One per pack and per log-segment object |

A full slot unlocks the master key, so the private key, so every data key. An agent slot unlocks only the metadata and chunk-ID keys, and the agent reads the public key from `keys/public`: enough to write, encrypt and deduplicate, never to read data.

## Constructions

`seal(K, P, AD)` is XChaCha20-Poly1305 with key `K`, a random 24-byte nonce, plaintext `P` and associated data `AD`, written as `nonce || ciphertext || tag`. Associated data always starts with a purpose label and a zero byte, so a ciphertext made for one purpose never opens as another:

| Object | Construction |
| --- | --- |
| Chunk | `seal(data key, header-byte ‖ chunk, "pentymento chunk v1" ‖ 0x00 ‖ chunk ID)` |
| Metadata object | `seal(metadata key, data, "pentymento metadata v1" ‖ 0x00 ‖ object key)` |
| `keys/private` | `seal(master key, age identity "AGE-SECRET-KEY-1…", "pentymento private key v1" ‖ 0x00)` |
| `keys/public` | `seal(metadata key, age recipient "age1…", "pentymento public key v1" ‖ 0x00)` |
| Passphrase or paper slot | `seal(Argon2id(secret, salt), payload, "pentymento slot v1" ‖ 0x00 ‖ slot ID)` |
| Age slot | age v1 file encrypted to the slot's X25519 recipient, holding the payload |
| Wrapped data key | age v1 file encrypted to the repository's X25519 recipient, holding the 32-byte data key |

Binding chunks to their IDs and metadata to their object keys means someone with write access to the bucket cannot swap one ciphertext for another without decryption failing.

### Sealed objects (log segments)

A log segment carries its own wrapped data key, so each segment can be read, expired or re-wrapped on its own:

```text
"PTMo" | version (1 byte, = 1) | wrapped-key length (uint16, big-endian) | wrapped key | seal(data key, segment, "pentymento object v1" ‖ 0x00 ‖ object key)
```

### Slot payload

The plaintext inside every slot:

```text
"PTMk" | version (1 byte, = 1) | scope (1 = full, 2 = agent) | slot ID (8 bytes) | [master key] | metadata key | chunk-ID key
```

The master key is present only in full slots. The slot ID inside the payload must match the slot's own ID, so a slot's ciphertext copied into another slot does not unlock.

### Key slots

Slots are JSON files in `keys/`:

```json
{
  "version": 1,
  "id": "9f2c41d07ab35e18",
  "kind": "passphrase",
  "scope": "full",
  "label": "laptop",
  "created": "2026-11-25T13:00:00Z",
  "kdf": {"name": "argon2id", "memory_kib": 65536, "time": 3, "threads": 4, "salt": "…base64…"},
  "ciphertext": "…base64…"
}
```

| Kind | Unlocked with | Scope |
| --- | --- | --- |
| `passphrase` | A passphrase, through Argon2id | Full or agent |
| `age` | An age X25519 identity (`AGE-SECRET-KEY-1…`), for automation | Full or agent |
| `paper` | A printed recovery code, through Argon2id | Full only |

- **Argon2id** uses RFC 9106's second recommended parameters: 64 MiB, 3 passes, 4 lanes, a 16-byte random salt. The parameters are stored in each slot. A slot asking for more than 4 GiB or 16 passes is refused before any derivation, so a tampered slot cannot exhaust memory.
- **The paper recovery code** is 200 random bits printed as 8 groups of 5 base32 characters, such as `ABCDE-FGHIJ-…`. It is shown once and never stored. Case, dashes and spaces are ignored when it is typed.

## Write path (agent)

1. Read `keys/public` with the metadata key.
2. For each pack, generate a random data key; encrypt each chunk with it; wrap the data key to the public key and store the wrapped key in the pack's index entry.
3. For each log segment, generate a data key, encrypt the segment, and store the wrapped key in the segment's header.
4. Discard every data key.

Deduplication still works: chunk IDs are keyed hashes of plaintext, compared against the index, which the agent can read with the metadata key, without decrypting any chunk.

## Read path (restore, verify, prune)

Unlock a full slot → master key → open `keys/private` → private key → unwrap data keys → decrypt chunks and segments.

## Key rotation

`pentym key rotate` generates a new key pair, re-wraps every data key in the index and every log segment's header to the new public key, and replaces `keys/private` and `keys/public`. Pack and segment ciphertext is never rewritten. Slots are unchanged, because they protect the master key, not the key pair.

## Threat model

| Threat | Protection |
| --- | --- |
| Database host compromised | The agent holds no private key and cannot read history; with write-only storage credentials it cannot delete it either |
| Bucket contents leaked | Every chunk, metadata object and segment is encrypted; chunk IDs are keyed |
| Bucket writer swaps or moves objects | Associated data binds each ciphertext to its chunk ID or object key; `keys/public` is authenticated |
| A tampered slot | Slot ID binding; bounded key-derivation parameters |
| Stale or substituted public key | `keys/public` is authenticated with the metadata key |

### What is not hidden

- Object sizes, counts and timing.
- Chunk equality, to anyone holding the chunk-ID key (agents and admins).
- Compressed sizes reveal a little about content.
- Without the chunk-ID key, chunk sizes of known content cannot be predicted, because the chunker's gear table is derived from that key; packed object sizes are still visible.

## Limitations

- **Removing a slot** stops that way of unlocking. It cannot revoke access already gained: someone who learned that slot's secret may have copied the master key, so after a suspected slot compromise, rotate the key pair and treat the master key as exposed.
- **Rotation re-wraps data keys but does not re-encrypt data.** Someone holding the old private key and an old copy of the index could still decrypt data written before the rotation.
- **Memory:** Go's garbage collector cannot guarantee that keys are wiped from memory; Pentymento zeroes the buffers it controls.
- **Losing every full slot and the paper code** makes every backup unreadable, by design. `init` requires two full slots or one plus a paper code; `pentym key remove` refuses to remove the last full slot.
- **`--insecure-test-mode`** creates an unencrypted repository for local development; it is refused for anything but a local path.
