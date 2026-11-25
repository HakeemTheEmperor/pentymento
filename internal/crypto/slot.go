// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"bytes"
	"crypto/rand"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"filippo.io/age"
	"golang.org/x/crypto/argon2"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Message IDs for keys (see docs/errors).
const (
	IDUnlockFailed     msg.ID = "PTM-4001"
	IDLastFullSlot     msg.ID = "PTM-4002"
	IDInsecureNotLocal msg.ID = "PTM-4003"
)

// Kind is how a slot is unlocked.
type Kind string

// Slot kinds.
const (
	// KindPassphrase is unlocked with a passphrase, through Argon2id.
	KindPassphrase Kind = "passphrase"
	// KindAge is unlocked with an age X25519 identity, for automation.
	KindAge Kind = "age"
	// KindPaper is a full slot unlocked with a printed recovery code.
	KindPaper Kind = "paper"
)

// Scope is what a slot gives access to.
type Scope string

// Slot scopes.
const (
	// ScopeFull unlocks everything: the master key (so the private key),
	// the metadata key and the chunk-ID key.
	ScopeFull Scope = "full"
	// ScopeAgent unlocks only the metadata and chunk-ID keys: enough to
	// write and deduplicate, never to read data.
	ScopeAgent Scope = "agent"
)

// KDFParams are a passphrase slot's Argon2id parameters, stored in the slot
// so they can change without breaking existing slots.
type KDFParams struct {
	Name      string `json:"name"`
	MemoryKiB uint32 `json:"memory_kib"`
	Time      uint32 `json:"time"`
	Threads   uint8  `json:"threads"`
	Salt      []byte `json:"salt"`
}

// DefaultKDF is RFC 9106's second recommended Argon2id option: 64 MiB,
// 3 passes, 4 lanes. The salt is filled in per slot.
var DefaultKDF = KDFParams{Name: "argon2id", MemoryKiB: 64 << 10, Time: 3, Threads: 4}

// Limits on stored KDF parameters, so a tampered slot cannot make an
// unlock exhaust memory or time.
const (
	minMemoryKiB = 8 << 10
	maxMemoryKiB = 4 << 20
	maxTime      = 16
	saltSize     = 16
)

func (p KDFParams) validate() error {
	if p.Name != "argon2id" || p.MemoryKiB < minMemoryKiB || p.MemoryKiB > maxMemoryKiB ||
		p.Time < 1 || p.Time > maxTime || p.Threads < 1 || len(p.Salt) != saltSize {
		return fmt.Errorf("unsupported key derivation parameters")
	}
	return nil
}

func (p KDFParams) derive(pass []byte) Key {
	k := argon2.IDKey(pass, p.Salt, p.Time, p.MemoryKiB, p.Threads, KeySize)
	key, _ := KeyFromBytes(k)
	clear(k)
	return key
}

// slotVersion is the slot format version.
const slotVersion = 1

// Slot is one way to unlock a repository's keys, stored in keys/. Only
// Ciphertext is secret, and it is encrypted.
type Slot struct {
	Version    int        `json:"version"`
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	Scope      Scope      `json:"scope"`
	Label      string     `json:"label"`
	Created    time.Time  `json:"created"`
	KDF        *KDFParams `json:"kdf,omitempty"`
	Recipient  string     `json:"recipient,omitempty"`
	Ciphertext []byte     `json:"ciphertext"`
}

// Unlocked is what a slot holds. Master is zero for an agent slot.
type Unlocked struct {
	Scope    Scope
	Master   Key
	Metadata Key
	ChunkID  Key
}

// Agent returns the agent-scope subset of u.
func (u Unlocked) Agent() Unlocked {
	return Unlocked{Scope: ScopeAgent, Metadata: u.Metadata, ChunkID: u.ChunkID}
}

// Slot payload: "PTMk" | version | scope (1 full, 2 agent) | slot ID (8) |
// keys (full: master, metadata, chunk ID; agent: metadata, chunk ID).
var payloadMagic = []byte("PTMk")

const (
	payloadVersion = 1
	idSize         = 8
)

func (u Unlocked) payload(id []byte) ([]byte, error) {
	out := append([]byte(nil), payloadMagic...)
	out = append(out, payloadVersion)
	switch u.Scope {
	case ScopeFull:
		if u.Master.IsZero() {
			return nil, errors.New("a full slot needs the master key")
		}
		out = append(out, 1)
		out = append(out, id...)
		out = append(out, u.Master.Bytes()...)
	case ScopeAgent:
		if !u.Master.IsZero() {
			return nil, errors.New("an agent slot must not hold the master key")
		}
		out = append(out, 2)
		out = append(out, id...)
	default:
		return nil, fmt.Errorf("unknown scope %q", u.Scope)
	}
	if u.Metadata.IsZero() || u.ChunkID.IsZero() {
		return nil, errors.New("a slot needs the metadata and chunk-ID keys")
	}
	out = append(out, u.Metadata.Bytes()...)
	return append(out, u.ChunkID.Bytes()...), nil
}

func parsePayload(b, id []byte, scope Scope) (*Unlocked, error) {
	h := len(payloadMagic) + 2
	if len(b) < h+idSize || !bytes.Equal(b[:len(payloadMagic)], payloadMagic) || b[len(payloadMagic)] != payloadVersion {
		return nil, ErrDecrypt
	}
	want, keys := byte(1), 3
	if scope == ScopeAgent {
		want, keys = 2, 2
	}
	if b[h-1] != want || !bytes.Equal(b[h:h+idSize], id) || len(b) != h+idSize+keys*KeySize {
		return nil, ErrDecrypt
	}
	rest := b[h+idSize:]
	u := &Unlocked{Scope: scope}
	if scope == ScopeFull {
		u.Master, _ = KeyFromBytes(rest[:KeySize])
		rest = rest[KeySize:]
	}
	u.Metadata, _ = KeyFromBytes(rest[:KeySize])
	u.ChunkID, _ = KeyFromBytes(rest[KeySize:])
	return u, nil
}

func newSlotBase(kind Kind, scope Scope, label string) (*Slot, []byte) {
	id := make([]byte, idSize)
	if _, err := rand.Read(id); err != nil {
		panic(err)
	}
	return &Slot{
		Version: slotVersion, ID: hex.EncodeToString(id), Kind: kind, Scope: scope,
		Label: label, Created: time.Now().UTC().Truncate(time.Second),
	}, id
}

// NewPassphraseSlot creates a slot unlocked with pass. kdf's salt is
// generated.
func NewPassphraseSlot(label string, keys Unlocked, pass secrets.Secret, kdf KDFParams) (*Slot, error) {
	return newPassphraseSlot(KindPassphrase, label, keys, pass.Expose(), kdf)
}

func newPassphraseSlot(kind Kind, label string, keys Unlocked, pass []byte, kdf KDFParams) (*Slot, error) {
	if len(pass) == 0 {
		return nil, errors.New("empty passphrase")
	}
	s, id := newSlotBase(kind, keys.Scope, label)
	kdf.Salt = make([]byte, saltSize)
	if _, err := rand.Read(kdf.Salt); err != nil {
		return nil, err
	}
	if err := kdf.validate(); err != nil {
		return nil, err
	}
	p, err := keys.payload(id)
	if err != nil {
		return nil, err
	}
	defer clear(p)
	kek := kdf.derive(pass)
	defer kek.Clear()
	s.KDF = &kdf
	s.Ciphertext = seal(kek, p, aad(purposeSlot, id))
	return s, nil
}

// NewAgeSlot creates a slot unlocked with the age identity matching
// recipient ("age1…").
func NewAgeSlot(label string, keys Unlocked, recipient string) (*Slot, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("recipient must be an age X25519 public key (age1…): %w", err)
	}
	s, id := newSlotBase(KindAge, keys.Scope, label)
	p, err := keys.payload(id)
	if err != nil {
		return nil, err
	}
	defer clear(p)
	var buf bytes.Buffer
	w, err := age.Encrypt(&buf, r)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(p); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	s.Recipient = r.String()
	s.Ciphertext = buf.Bytes()
	return s, nil
}

// paperBytes is the paper code's entropy: 200 bits, printed as 8 groups of
// 5 base32 characters.
const paperBytes = 25

// NewPaperSlot creates a full slot unlocked with a random recovery code,
// which it returns for printing; the code is not stored anywhere.
func NewPaperSlot(label string, keys Unlocked, kdf KDFParams) (*Slot, secrets.Secret, error) {
	if keys.Scope != ScopeFull {
		return nil, secrets.Secret{}, errors.New("a paper key must be a full slot")
	}
	raw := make([]byte, paperBytes)
	if _, err := rand.Read(raw); err != nil {
		return nil, secrets.Secret{}, err
	}
	defer clear(raw)
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	var groups []string
	for i := 0; i < len(code); i += 5 {
		groups = append(groups, code[i:i+5])
	}
	printed := secrets.New([]byte(strings.Join(groups, "-")))
	s, err := newPassphraseSlot(KindPaper, label, keys, []byte(code), kdf)
	if err != nil {
		return nil, secrets.Secret{}, err
	}
	return s, printed, nil
}

// normalizePaper turns a typed recovery code into the stored form:
// upper case, without dashes or spaces.
func normalizePaper(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		switch {
		case c == '-' || c == ' ' || c == '\t' || c == '\n' || c == '\r':
		case c >= 'a' && c <= 'z':
			out = append(out, c-'a'+'A')
		default:
			out = append(out, c)
		}
	}
	return out
}

// UnlockPassphrase opens a passphrase or paper slot.
func (s *Slot) UnlockPassphrase(pass secrets.Secret) (*Unlocked, error) {
	if s.Kind != KindPassphrase && s.Kind != KindPaper {
		return nil, fmt.Errorf("slot %s is a %s slot, not a passphrase slot", s.ID, s.Kind)
	}
	if s.KDF == nil {
		return nil, fmt.Errorf("slot %s has no key derivation parameters", s.ID)
	}
	if err := s.KDF.validate(); err != nil {
		return nil, fmt.Errorf("slot %s: %w", s.ID, err)
	}
	id, err := hex.DecodeString(s.ID)
	if err != nil || len(id) != idSize {
		return nil, fmt.Errorf("slot ID %q is malformed", s.ID)
	}
	p := pass.Expose()
	if s.Kind == KindPaper {
		p = normalizePaper(p)
		defer clear(p)
	}
	kek := s.KDF.derive(p)
	defer kek.Clear()
	payload, err := open(kek, s.Ciphertext, aad(purposeSlot, id))
	if err != nil {
		return nil, s.unlockFailed()
	}
	defer clear(payload)
	u, err := parsePayload(payload, id, s.Scope)
	if err != nil {
		return nil, s.unlockFailed()
	}
	return u, nil
}

// UnlockAge opens an age slot with an age X25519 identity
// ("AGE-SECRET-KEY-1…").
func (s *Slot) UnlockAge(identity secrets.Secret) (*Unlocked, error) {
	if s.Kind != KindAge {
		return nil, fmt.Errorf("slot %s is a %s slot, not an age slot", s.ID, s.Kind)
	}
	id, err := hex.DecodeString(s.ID)
	if err != nil || len(id) != idSize {
		return nil, fmt.Errorf("slot ID %q is malformed", s.ID)
	}
	ident, err := age.ParseX25519Identity(strings.TrimSpace(string(identity.Expose())))
	if err != nil {
		return nil, s.unlockFailed()
	}
	r, err := age.Decrypt(bytes.NewReader(s.Ciphertext), ident)
	if err != nil {
		return nil, s.unlockFailed()
	}
	payload, err := io.ReadAll(io.LimitReader(r, 1024))
	if err != nil {
		return nil, s.unlockFailed()
	}
	defer clear(payload)
	u, err := parsePayload(payload, id, s.Scope)
	if err != nil {
		return nil, s.unlockFailed()
	}
	return u, nil
}

func (s *Slot) unlockFailed() error {
	what := "passphrase"
	switch s.Kind {
	case KindAge:
		what = "age identity"
	case KindPaper:
		what = "recovery code"
	}
	return &msg.Error{Code: msg.ExitAccess, Err: ErrDecrypt, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDUnlockFailed,
		Fact: fmt.Sprintf("key slot %s (%s) did not unlock: wrong %s", s.ID, s.Label, what),
		Next: "check the " + what + " for this slot, or unlock with another slot (pentym key list)",
	}}
}

// Marshal encodes the slot for storage in keys/.
func (s *Slot) Marshal() ([]byte, error) { return json.Marshal(s) }

// ParseSlot decodes and checks a stored slot.
func ParseSlot(data []byte) (*Slot, error) {
	var s Slot
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("key slot is malformed: %w", err)
	}
	if s.Version > slotVersion {
		return nil, fmt.Errorf("key slot version %d is newer than this pentym understands", s.Version)
	}
	switch {
	case s.Version != slotVersion:
		return nil, fmt.Errorf("key slot version %d is not supported", s.Version)
	case s.Kind != KindPassphrase && s.Kind != KindAge && s.Kind != KindPaper:
		return nil, fmt.Errorf("key slot kind %q is not supported", s.Kind)
	case s.Scope != ScopeFull && s.Scope != ScopeAgent:
		return nil, fmt.Errorf("key slot scope %q is not supported", s.Scope)
	case s.Kind == KindPaper && s.Scope != ScopeFull:
		return nil, errors.New("a paper slot must be a full slot")
	case (s.Kind == KindAge) != (s.Recipient != "") || (s.Kind == KindAge) == (s.KDF != nil):
		return nil, errors.New("key slot fields do not match its kind")
	}
	if id, err := hex.DecodeString(s.ID); err != nil || len(id) != idSize {
		return nil, fmt.Errorf("slot ID %q is malformed", s.ID)
	}
	return &s, nil
}
