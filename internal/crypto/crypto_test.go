// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package crypto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/HakeemTheEmperor/pentymento/internal/devtools/leaktest"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// fastKDF keeps tests quick; DefaultKDF is exercised once on its own.
var fastKDF = KDFParams{Name: "argon2id", MemoryKiB: minMemoryKiB, Time: 1, Threads: 1}

func mustMaterial(t *testing.T) *Material {
	t.Helper()
	m, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func pass(s string) secrets.Secret { return secrets.New([]byte(s)) }

// flip returns a copy of b with one bit flipped at i.
func flip(b []byte, i int) []byte {
	c := append([]byte(nil), b...)
	c[i] ^= 0x01
	return c
}

func TestKeys(t *testing.T) {
	a, b := NewKey(), NewKey()
	if bytes.Equal(a.Bytes(), b.Bytes()) || len(a.Bytes()) != KeySize {
		t.Fatal("NewKey is not random or not 32 bytes")
	}
	if _, err := KeyFromBytes(make([]byte, 31)); err == nil {
		t.Error("KeyFromBytes accepted 31 bytes")
	}
	c, _ := KeyFromBytes(a.Bytes())
	if c.Array() != a.Array() {
		t.Error("KeyFromBytes changed the key")
	}
	c.Clear()
	if c.Array() != [KeySize]byte{} || a.Array() == [KeySize]byte{} {
		t.Error("Clear did not zero only its own copy")
	}
	if !(Key{}).IsZero() || (Key{}).Bytes() != nil || !(PrivateKey{}).IsZero() || !(PublicKey{}).IsZero() {
		t.Error("zero keys are not zero")
	}
}

// TestSecretLeakKeys prints every key type through every formatting path.
func TestSecretLeakKeys(t *testing.T) {
	m := mustMaterial(t)
	type holder struct {
		key  Key
		priv PrivateKey
		u    Unlocked
	}
	h := holder{key: m.Master, priv: m.Private, u: m.Unlocked()}
	var outs []string
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		outs = append(outs, fmt.Sprintf(verb, m), fmt.Sprintf(verb, *m), fmt.Sprintf(verb, h), fmt.Sprintf(verb, &h),
			fmt.Sprintf(verb, m.Private), fmt.Sprintf(verb, m.Master))
	}
	j, _ := json.Marshal(m)
	outs = append(outs, string(j))
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("keys", "m", m, "priv", m.Private, "key", m.Master, "u", m.Unlocked())
	outs = append(outs, buf.String())

	priv := m.Private.identity().String()
	for _, k := range []Key{m.Master, m.Metadata, m.ChunkID} {
		for _, o := range outs {
			if bytes.Contains([]byte(o), k.Bytes()) || strings.Contains(o, fmt.Sprintf("%x", k.Bytes())) ||
				strings.Contains(o, strings.Trim(fmt.Sprintf("%d", k.Bytes()), "[]")) {
				t.Fatalf("key bytes leaked in %q", o)
			}
		}
	}
	for _, o := range outs {
		if strings.Contains(strings.ToUpper(o), "AGE-SECRET-KEY") || strings.Contains(o, priv) {
			t.Fatalf("private key leaked in %q", o)
		}
	}
	leaktest.Scan(t, "key formatting", []leaktest.Canary{leaktest.Canary(priv)}, outs...)
}

func TestChunkEncryption(t *testing.T) {
	dk := NewKey()
	id := [32]byte{1, 2, 3}
	plain := []byte("\x01compressed chunk bytes")
	sealed := EncryptChunk(dk, id, plain)
	if len(sealed) != len(plain)+Overhead {
		t.Errorf("sealed length %d, want %d", len(sealed), len(plain)+Overhead)
	}
	if bytes.Equal(EncryptChunk(dk, id, plain), sealed) {
		t.Error("nonces repeat")
	}
	got, err := DecryptChunk(dk, id, sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v", err)
	}
	for i := range sealed {
		if _, err := DecryptChunk(dk, id, flip(sealed, i)); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	if _, err := DecryptChunk(NewKey(), id, sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("wrong key accepted")
	}
	other := id
	other[31] = 9
	if _, err := DecryptChunk(dk, other, sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("chunk opened under another chunk's ID")
	}
	if _, err := DecryptChunk(dk, id, sealed[:Overhead-1]); !errors.Is(err, ErrDecrypt) {
		t.Error("truncated chunk accepted")
	}
	if _, err := DecryptChunk(Key{}, id, sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("zero key accepted")
	}
}

func TestMetadataEncryption(t *testing.T) {
	mk := NewKey()
	plain := []byte(`{"target":"prod-pg"}`)
	sealed := EncryptMetadata(mk, "snapshots/abc", plain)
	got, err := DecryptMetadata(mk, "snapshots/abc", sealed)
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := DecryptMetadata(mk, "snapshots/def", sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("metadata opened under another object key")
	}
	if _, err := DecryptMetadata(NewKey(), "snapshots/abc", sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("wrong key accepted")
	}
	// A chunk ciphertext never opens as metadata, even with the same key
	// bytes and identity, because the purposes differ.
	var id [32]byte
	copy(id[:], "snapshots/abc")
	if _, err := DecryptMetadata(mk, "snapshots/abc", EncryptChunk(mk, id, plain)); !errors.Is(err, ErrDecrypt) {
		t.Error("a chunk opened as metadata")
	}
}

func TestWrapKey(t *testing.T) {
	m := mustMaterial(t)
	dk := NewKey()
	wrapped, err := WrapKey(m.Public(), dk)
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnwrapKey(m.Private, wrapped)
	if err != nil || got.Array() != dk.Array() {
		t.Fatalf("unwrap: %v", err)
	}
	other := mustMaterial(t)
	if _, err := UnwrapKey(other.Private, wrapped); !errors.Is(err, ErrDecrypt) {
		t.Error("another private key unwrapped the data key")
	}
	if _, err := UnwrapKey(m.Private, flip(wrapped, len(wrapped)-1)); !errors.Is(err, ErrDecrypt) {
		t.Error("tampered wrapped key accepted")
	}
	if _, err := WrapKey(PublicKey{}, dk); err == nil {
		t.Error("wrapped to a zero public key")
	}
	t.Logf("wrapped data key: %d bytes", len(wrapped))
}

func TestSealedObject(t *testing.T) {
	m := mustMaterial(t)
	seg := bytes.Repeat([]byte("WAL record "), 1000)
	sealed, err := SealObject(m.Public(), "wal/t1/00000001/seg", seg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := OpenObject(m.Private, "wal/t1/00000001/seg", sealed)
	if err != nil || !bytes.Equal(got, seg) {
		t.Fatalf("round trip: %v", err)
	}
	if _, err := OpenObject(m.Private, "wal/t1/00000001/other", sealed); !errors.Is(err, ErrDecrypt) {
		t.Error("object opened under another key")
	}
	for _, i := range []int{0, 5, 30, len(sealed) - 1} {
		if _, err := OpenObject(m.Private, "wal/t1/00000001/seg", flip(sealed, i)); err == nil {
			t.Errorf("tampered byte %d accepted", i)
		}
	}
	newer := append([]byte(nil), sealed...)
	newer[4] = 2
	if _, err := OpenObject(m.Private, "wal/t1/00000001/seg", newer); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Errorf("newer version: %v", err)
	}
	for _, bad := range [][]byte{nil, []byte("PTMo"), []byte("XXXX\x01\x00\x00"), []byte("PTMo\x01\xff\xff")} {
		if _, err := OpenObject(m.Private, "k", bad); err == nil {
			t.Errorf("malformed object %q accepted", bad)
		}
	}
}

// TestAgentCannotDecrypt: an agent holds only what an agent slot unlocks
// plus the public key. It can encrypt, wrap and write metadata, but no
// path reaches a data key or the private key.
func TestAgentCannotDecrypt(t *testing.T) {
	m := mustMaterial(t)
	privObj := SealPrivate(m.Master, m.Private)
	pubObj := SealPublic(m.Metadata, m.Public())

	slot, err := NewPassphraseSlot("agent", m.Unlocked().Agent(), pass("agent secret"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := slot.UnlockPassphrase(pass("agent secret"))
	if err != nil {
		t.Fatal(err)
	}
	if agent.Scope != ScopeAgent || !agent.Master.IsZero() {
		t.Fatal("agent slot yielded the master key")
	}

	// What the agent can do: find the public key, encrypt and wrap.
	pub, err := OpenPublic(agent.Metadata, pubObj)
	if err != nil {
		t.Fatalf("agent cannot read keys/public: %v", err)
	}
	dk := NewKey()
	wrapped, err := WrapKey(pub, dk)
	if err != nil {
		t.Fatal(err)
	}
	var id [32]byte
	chunk := EncryptChunk(dk, id, []byte("row data"))
	segment, err := SealObject(pub, "wal/x", []byte("wal"))
	if err != nil {
		t.Fatal(err)
	}
	dk.Clear() // the agent discards the data key

	// What it cannot do: every key it holds fails on every secret.
	for name, k := range map[string]Key{"metadata": agent.Metadata, "chunk-ID": agent.ChunkID, "zero master": agent.Master} {
		if _, err := OpenPrivate(k, privObj); err == nil {
			t.Errorf("the agent's %s key opened keys/private", name)
		}
		if _, err := DecryptChunk(k, id, chunk); err == nil {
			t.Errorf("the agent's %s key decrypted a chunk", name)
		}
	}
	if _, err := UnwrapKey(PrivateKey{}, wrapped); err == nil {
		t.Error("a data key unwrapped without the private key")
	}
	if _, err := OpenObject(PrivateKey{}, "wal/x", segment); err == nil {
		t.Error("a segment opened without the private key")
	}
	// The agent slot's plaintext holds no master key at all.
	kek := slot.KDF.derive([]byte("agent secret"))
	idb := mustHex(t, slot.ID)
	payload, err := open(kek, slot.Ciphertext, aad(purposeSlot, idb))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(payload, m.Master.Bytes()) || len(payload) != len(payloadMagic)+2+idSize+2*KeySize {
		t.Error("the agent slot's plaintext contains the master key")
	}

	// A full unlock reads everything the agent wrote.
	priv, err := OpenPrivate(m.Master, privObj)
	if err != nil {
		t.Fatal(err)
	}
	dk2, err := UnwrapKey(priv, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := DecryptChunk(dk2, id, chunk); err != nil || string(got) != "row data" {
		t.Errorf("full unlock cannot read the agent's chunk: %v", err)
	}
	if got, err := OpenObject(priv, "wal/x", segment); err != nil || string(got) != "wal" {
		t.Errorf("full unlock cannot read the agent's segment: %v", err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b := make([]byte, len(s)/2)
	for i := range b {
		v, err := strconv.ParseUint(s[2*i:2*i+2], 16, 8)
		if err != nil {
			t.Fatal(err)
		}
		b[i] = byte(v)
	}
	return b
}

func TestPrivateAndPublicObjects(t *testing.T) {
	m := mustMaterial(t)
	privObj := SealPrivate(m.Master, m.Private)
	priv, err := OpenPrivate(m.Master, privObj)
	if err != nil || priv.Public().String() != m.Public().String() {
		t.Fatalf("keys/private round trip: %v", err)
	}
	if _, err := OpenPrivate(NewKey(), privObj); !errors.Is(err, ErrDecrypt) {
		t.Error("keys/private opened with another master key")
	}
	pubObj := SealPublic(m.Metadata, m.Public())
	pub, err := OpenPublic(m.Metadata, pubObj)
	if err != nil || pub.String() != m.Public().String() || !strings.HasPrefix(pub.String(), "age1") {
		t.Fatalf("keys/public round trip: %v (%s)", err, pub)
	}
	// A bucket writer without the metadata key cannot substitute a key.
	attacker := mustMaterial(t)
	if _, err := OpenPublic(m.Metadata, SealPublic(attacker.Metadata, attacker.Public())); !errors.Is(err, ErrDecrypt) {
		t.Error("a substituted public key was accepted")
	}
	if _, err := OpenPrivate(m.Metadata, privObj); !errors.Is(err, ErrDecrypt) {
		t.Error("keys/private opened with the metadata key")
	}
}

// TestRotation: rotation re-wraps data keys and segment headers to the new
// key pair, never touches chunk or segment ciphertext, and leaves slots
// working.
func TestRotation(t *testing.T) {
	m := mustMaterial(t)
	slot, err := NewPassphraseSlot("admin", m.Unlocked(), pass("correct horse"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	var wrappedKeys, chunks [][]byte
	var ids [][32]byte
	for i := range 5 {
		dk := NewKey()
		w, err := WrapKey(m.Public(), dk)
		if err != nil {
			t.Fatal(err)
		}
		id := [32]byte{byte(i)}
		wrappedKeys = append(wrappedKeys, w)
		chunks = append(chunks, EncryptChunk(dk, id, []byte(fmt.Sprint("chunk ", i))))
		ids = append(ids, id)
	}
	segment, err := SealObject(m.Public(), "wal/s", []byte("segment"))
	if err != nil {
		t.Fatal(err)
	}
	chunksBefore := fmt.Sprintf("%x", chunks)

	rot, err := Rotate(m)
	if err != nil {
		t.Fatal(err)
	}
	newPub := rot.Private.Public()
	if newPub.String() == m.Public().String() {
		t.Fatal("rotation kept the key pair")
	}
	for i, w := range wrappedKeys {
		if wrappedKeys[i], err = RewrapKey(m.Private, newPub, w); err != nil {
			t.Fatal(err)
		}
	}
	newSegment, err := RewrapObject(m.Private, newPub, segment)
	if err != nil {
		t.Fatal(err)
	}
	_, bodyBefore, _ := splitObject(segment)
	_, bodyAfter, _ := splitObject(newSegment)
	if !bytes.Equal(bodyBefore, bodyAfter) {
		t.Error("rotation rewrote the segment's ciphertext")
	}
	if fmt.Sprintf("%x", chunks) != chunksBefore {
		t.Error("rotation touched chunk ciphertext")
	}

	// The unchanged slot still unlocks, and its master key opens the new
	// keys/private, which reads everything.
	u, err := slot.UnlockPassphrase(pass("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := OpenPrivate(u.Master, rot.PrivateObject)
	if err != nil {
		t.Fatal(err)
	}
	if pub, err := OpenPublic(u.Metadata, rot.PublicObject); err != nil || pub.String() != newPub.String() {
		t.Fatalf("new keys/public: %v", err)
	}
	for i, w := range wrappedKeys {
		dk, err := UnwrapKey(priv, w)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DecryptChunk(dk, ids[i], chunks[i]); err != nil || string(got) != fmt.Sprint("chunk ", i) {
			t.Errorf("chunk %d unreadable after rotation", i)
		}
		if _, err := UnwrapKey(m.Private, w); err == nil {
			t.Errorf("the old private key still unwraps data key %d", i)
		}
	}
	if got, err := OpenObject(priv, "wal/s", newSegment); err != nil || string(got) != "segment" {
		t.Error("segment unreadable after rotation")
	}
	if _, err := Rotate(&Material{Metadata: m.Metadata}); err == nil {
		t.Error("rotated without the master key")
	}
}

func TestPassphraseSlot(t *testing.T) {
	m := mustMaterial(t)
	s, err := NewPassphraseSlot("laptop", m.Unlocked(), pass("correct horse battery staple"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	u, err := s.UnlockPassphrase(pass("correct horse battery staple"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scope != ScopeFull || u.Master.Array() != m.Master.Array() || u.Metadata.Array() != m.Metadata.Array() || u.ChunkID.Array() != m.ChunkID.Array() {
		t.Fatal("unlocked keys differ")
	}
	_, err = s.UnlockPassphrase(pass("wrong"))
	var me *msg.Error
	if !errors.As(err, &me) || me.Code != msg.ExitAccess || me.Message.ID != IDUnlockFailed || !errors.Is(err, ErrDecrypt) {
		t.Errorf("wrong passphrase: %v", err)
	}
	if _, err := NewPassphraseSlot("x", m.Unlocked(), pass(""), fastKDF); err == nil {
		t.Error("empty passphrase accepted")
	}
	if _, err := s.UnlockAge(pass("x")); err == nil {
		t.Error("UnlockAge on a passphrase slot")
	}
}

func TestDefaultKDF(t *testing.T) {
	if testing.Short() {
		t.Skip("64 MiB key derivation")
	}
	m := mustMaterial(t)
	s, err := NewPassphraseSlot("default", m.Unlocked(), pass("pw"), DefaultKDF)
	if err != nil {
		t.Fatal(err)
	}
	if s.KDF.MemoryKiB != 65536 || s.KDF.Time != 3 || s.KDF.Threads != 4 || len(s.KDF.Salt) != 16 {
		t.Errorf("KDF = %+v", s.KDF)
	}
	start := time.Now()
	if _, err := s.UnlockPassphrase(pass("pw")); err != nil {
		t.Fatal(err)
	}
	t.Logf("unlock with the default Argon2id parameters took %s", time.Since(start))
}

func TestAgeSlot(t *testing.T) {
	m := mustMaterial(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewAgeSlot("ci", m.Unlocked().Agent(), id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if s.Recipient != id.Recipient().String() || s.KDF != nil {
		t.Errorf("age slot fields: %+v", s)
	}
	u, err := s.UnlockAge(secrets.New([]byte(id.String() + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if u.Scope != ScopeAgent || !u.Master.IsZero() || u.Metadata.Array() != m.Metadata.Array() {
		t.Error("age agent slot keys differ")
	}
	other, _ := age.GenerateX25519Identity()
	for _, bad := range []string{other.String(), "not an identity", ""} {
		if _, err := s.UnlockAge(secrets.New([]byte(bad))); !errors.Is(err, ErrDecrypt) {
			t.Errorf("age slot unlocked with %q: %v", bad, err)
		}
	}
	if _, err := NewAgeSlot("x", m.Unlocked(), "age1notarecipient"); err == nil {
		t.Error("bad recipient accepted")
	}
	if _, err := s.UnlockPassphrase(pass("x")); err == nil {
		t.Error("UnlockPassphrase on an age slot")
	}
}

var paperRE = regexp.MustCompile(`^[A-Z2-7]{5}(-[A-Z2-7]{5}){7}$`)

func TestPaperSlot(t *testing.T) {
	m := mustMaterial(t)
	s, code, err := NewPaperSlot("paper", m.Unlocked(), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	c := string(code.Expose())
	if !paperRE.MatchString(c) {
		t.Fatalf("paper code %q is not 8 groups of 5 base32 characters", c)
	}
	for _, typed := range []string{c, strings.ToLower(c), strings.ReplaceAll(c, "-", " "), strings.ReplaceAll(c, "-", "") + "\n"} {
		u, err := s.UnlockPassphrase(pass(typed))
		if err != nil || u.Master.Array() != m.Master.Array() {
			t.Errorf("paper code typed as %q did not unlock: %v", typed, err)
		}
	}
	if _, err := s.UnlockPassphrase(pass(strings.Replace(c, c[:1], "Z", 1) + "A")); err == nil {
		t.Error("wrong code unlocked")
	}
	if _, _, err := NewPaperSlot("x", m.Unlocked().Agent(), fastKDF); err == nil {
		t.Error("agent-scope paper slot created")
	}
	if fmt.Sprint(code) != "[redacted]" {
		t.Error("the paper code prints")
	}
}

func TestSlotSerialization(t *testing.T) {
	m := mustMaterial(t)
	s, err := NewPassphraseSlot("laptop", m.Unlocked(), pass("pw"), fastKDF)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseSlot(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := back.UnlockPassphrase(pass("pw")); err != nil {
		t.Fatalf("parsed slot does not unlock: %v", err)
	}
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	for _, k := range []string{"version", "id", "kind", "scope", "label", "created", "kdf", "ciphertext"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("serialized slot lacks %q", k)
		}
	}

	mutate := func(f func(map[string]any)) []byte {
		var v map[string]any
		_ = json.Unmarshal(b, &v)
		f(v)
		out, _ := json.Marshal(v)
		return out
	}
	bad := map[string][]byte{
		"newer version":   mutate(func(v map[string]any) { v["version"] = 2 }),
		"unknown kind":    mutate(func(v map[string]any) { v["kind"] = "kms" }),
		"unknown scope":   mutate(func(v map[string]any) { v["scope"] = "root" }),
		"unknown field":   mutate(func(v map[string]any) { v["extra"] = 1 }),
		"bad id":          mutate(func(v map[string]any) { v["id"] = "xyz" }),
		"age with kdf":    mutate(func(v map[string]any) { v["kind"] = "age"; v["recipient"] = "age1x" }),
		"passphrase no k": mutate(func(v map[string]any) { delete(v, "kdf") }),
		"not json":        []byte("{"),
	}
	for name, data := range bad {
		if _, err := ParseSlot(data); err == nil {
			t.Errorf("%s: ParseSlot accepted it", name)
		}
	}

	// Tampered KDF parameters are refused before any key derivation.
	huge := mutate(func(v map[string]any) { v["kdf"].(map[string]any)["memory_kib"] = 1 << 30 })
	hs, err := ParseSlot(huge)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hs.UnlockPassphrase(pass("pw")); err == nil || !strings.Contains(err.Error(), "unsupported key derivation") {
		t.Errorf("huge memory parameter: %v", err)
	}

	// A slot's ciphertext moved into another slot does not unlock: the ID
	// is bound in.
	s2, _ := NewPassphraseSlot("other", m.Unlocked(), pass("pw"), fastKDF)
	s2.KDF, s2.Ciphertext = s.KDF, s.Ciphertext
	if _, err := s2.UnlockPassphrase(pass("pw")); err == nil {
		t.Error("ciphertext moved between slots still unlocked")
	}
}

func TestSlotRules(t *testing.T) {
	m := mustMaterial(t)
	full1, _ := NewPassphraseSlot("a", m.Unlocked(), pass("a"), fastKDF)
	full2, _, _ := NewPaperSlot("paper", m.Unlocked(), fastKDF)
	agent, _ := NewPassphraseSlot("agent", m.Unlocked().Agent(), pass("b"), fastKDF)

	if EnoughFullSlots([]*Slot{full1, agent}) || !EnoughFullSlots([]*Slot{full1, full2, agent}) {
		t.Error("EnoughFullSlots is wrong")
	}
	if FullSlots([]*Slot{full1, full2, agent}) != 2 {
		t.Error("FullSlots is wrong")
	}
	if err := CheckRemove([]*Slot{full1, full2, agent}, full1.ID); err != nil {
		t.Errorf("removing one of two full slots: %v", err)
	}
	if err := CheckRemove([]*Slot{full1, agent}, agent.ID); err != nil {
		t.Errorf("removing the agent slot: %v", err)
	}
	err := CheckRemove([]*Slot{full1, agent}, full1.ID)
	var me *msg.Error
	if !errors.As(err, &me) || me.Message.ID != IDLastFullSlot || me.Code != msg.ExitAccess {
		t.Errorf("removing the last full slot: %v", err)
	}
	if err := CheckRemove([]*Slot{full1}, "0000000000000000"); !errors.As(err, &me) || me.Code != msg.ExitNotFound {
		t.Errorf("removing a missing slot: %v", err)
	}
	if _, err := NewPassphraseSlot("bad", Unlocked{Scope: ScopeFull, Metadata: m.Metadata, ChunkID: m.ChunkID}, pass("x"), fastKDF); err == nil {
		t.Error("full slot without a master key")
	}
	if _, err := NewPassphraseSlot("bad", Unlocked{Scope: ScopeAgent, Master: m.Master, Metadata: m.Metadata, ChunkID: m.ChunkID}, pass("x"), fastKDF); err == nil {
		t.Error("agent slot holding the master key")
	}
}

func TestInsecureTestMode(t *testing.T) {
	if err := CheckInsecureTestMode(true); err != nil {
		t.Errorf("local: %v", err)
	}
	var me *msg.Error
	if err := CheckInsecureTestMode(false); !errors.As(err, &me) || me.Message.ID != IDInsecureNotLocal {
		t.Errorf("remote: %v", err)
	}
}

// TestPrimitivesOnly checks that the whole module takes its cryptography
// only from the approved libraries (PRD 8.2): golang.org/x/crypto,
// filippo.io/age and github.com/zeebo/blake3, plus crypto/rand for
// randomness.
func TestPrimitivesOnly(t *testing.T) {
	allowed := map[string]bool{
		"crypto/rand":                          true,
		"golang.org/x/crypto/chacha20poly1305": true,
		"golang.org/x/crypto/argon2":           true,
		"filippo.io/age":                       true,
		"github.com/zeebo/blake3":              true,
	}
	checked := 0
	err := filepath.WalkDir("../..", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) && p != "../.." {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		pf, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		checked++
		for _, imp := range pf.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			cryptoish := strings.HasPrefix(path, "crypto/") || strings.HasPrefix(path, "golang.org/x/crypto") ||
				strings.Contains(path, "/age") || strings.Contains(path, "blake3") ||
				strings.Contains(path, "chacha") || strings.Contains(path, "argon") || strings.Contains(path, "nacl")
			if cryptoish && !allowed[path] {
				t.Errorf("%s imports %s, which is not an approved cryptographic library", p, path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("checked only %d files; run from the package directory", checked)
	}
}

func FuzzParseSlot(f *testing.F) {
	m, _ := Generate()
	s, _ := NewPassphraseSlot("x", m.Unlocked(), pass("pw"), fastKDF)
	b, _ := s.Marshal()
	f.Add(b)
	f.Add([]byte(`{"version":1}`))
	f.Fuzz(func(_ *testing.T, data []byte) {
		s, err := ParseSlot(data)
		if err != nil {
			return
		}
		// Whatever parses must fail to unlock cleanly, never panic.
		if s.Kind == KindAge {
			_, _ = s.UnlockAge(pass("AGE-SECRET-KEY-1"))
		} else if s.KDF != nil && s.KDF.validate() == nil && s.KDF.MemoryKiB <= minMemoryKiB && s.KDF.Time <= 2 {
			_, _ = s.UnlockPassphrase(pass("pw"))
		}
	})
}

func FuzzOpenObject(f *testing.F) {
	m, _ := Generate()
	sealed, _ := SealObject(m.Public(), "k", []byte("data"))
	f.Add(sealed)
	f.Add([]byte("PTMo\x01\x00\x05hello"))
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = OpenObject(m.Private, "k", data)
		_, _ = DecryptChunk(m.Master, [32]byte{}, data)
		_, _ = OpenPrivate(m.Master, data)
		_, _ = OpenPublic(m.Metadata, data)
	})
}
