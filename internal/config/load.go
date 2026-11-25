// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/secrets"
)

// Message IDs for configuration problems (see docs/errors).
const (
	IDPlaintextRejected msg.ID = secrets.IDPlaintextRejected
	IDPlaintextReadable msg.ID = "PTM-1002"
	IDInvalid           msg.ID = "PTM-1003"
	IDNotFound          msg.ID = "PTM-1008"
)

const (
	includeKey   = "include"
	plaintextKey = "allow_plaintext_secrets"
)

// sources records where a config came from.
type sources struct {
	main  string   // the file named by the user
	files []string // every file read, in the order applied
}

// plaintextUse is a plaintext secret found in a config file.
type plaintextUse struct {
	file string
	line int
	path string
}

type loader struct {
	stack     []string // files being read, to detect include cycles
	files     []string
	plaintext []plaintextUse
	newValue  func() any // a new *T for per-file strict decoding
}

// load reads path and its includes into a T, applies environment
// overrides and enforces the plaintext-secret rules.
func load[T any](path string, environ []string) (*T, sources, error) {
	l := &loader{newValue: func() any { return new(T) }}
	root, err := l.read(path, "")
	if err != nil {
		return nil, sources{main: path}, err
	}
	src := sources{main: path, files: l.files}

	envPaths, err := applyEnv(root, reflect.TypeFor[T](), environ)
	if err != nil {
		return nil, src, err
	}

	out := new(T)
	if err := root.Decode(out); err != nil {
		line, text := yamlProblem(err)
		return nil, src, invalid(path, line, text)
	}

	var envPlain []string
	walkRefs(reflect.ValueOf(out).Elem(), nil, func(p []string, r secrets.Ref) {
		if v, ok := envPaths[strings.Join(p, ".")]; ok && r.IsPlaintext() {
			envPlain = append(envPlain, v)
		}
	})
	if len(envPlain) > 0 {
		sort.Strings(envPlain)
		return nil, src, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDPlaintextRejected,
			Fact:   "plaintext secret in environment variable " + envPlain[0],
			Impact: "an override of a secret must be a reference, never the secret itself",
			Next:   fmt.Sprintf("set %s to a reference such as env:NAME or file:/path", envPlain[0]),
		}}
	}

	if len(l.plaintext) > 0 {
		if !allowPlaintext(out) {
			u := l.plaintext[0]
			return nil, src, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
				Level: msg.LevelFailed, ID: IDPlaintextRejected,
				Fact:   fmt.Sprintf("plaintext secret in %s line %d (%s)", u.file, u.line, u.path),
				Impact: "secrets in the config must be references, so the file never holds them",
				Next:   "replace it with a reference such as env:NAME or file:/path, or set allow_plaintext_secrets: true",
			}}
		}
		checked := map[string]bool{}
		for _, u := range l.plaintext {
			if checked[u.file] {
				continue
			}
			checked[u.file] = true
			if err := checkPrivate(u.file); err != nil {
				return nil, src, err
			}
		}
	}
	return out, src, nil
}

// read parses one file, strictly checks it against T, and returns it merged
// over its includes.
func (l *loader) read(path, includedFrom string) (*yaml.Node, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, invalid(path, 0, err.Error())
	}
	for _, p := range l.stack {
		if sameFile(p, abs) {
			return nil, invalid(includedFrom, 0, fmt.Sprintf("include cycle: %s includes %s again", includedFrom, path))
		}
	}
	data, err := os.ReadFile(path) // #nosec G304 -- the operator's config file and its includes
	if err != nil {
		return nil, readError(path, includedFrom, err)
	}
	l.files = append(l.files, path)

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		line, text := yamlProblem(err)
		return nil, invalid(path, line, text)
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 {
		node = doc.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, invalid(path, node.Line, "the config must be a mapping of keys to values")
	}

	// Strict per-file decode: unknown keys and wrong types are reported
	// with this file's name and line.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	v := l.newValue()
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		line, text := yamlProblem(err)
		return nil, invalid(path, line, text)
	}
	walkRefs(reflect.ValueOf(v).Elem(), nil, func(p []string, r secrets.Ref) {
		if r.IsPlaintext() {
			l.plaintext = append(l.plaintext, plaintextUse{file: path, line: r.Line, path: strings.Join(p, ".")})
		}
	})

	includes := removeKey(node, includeKey)
	merged := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	if includes != nil {
		l.stack = append(l.stack, abs)
		for _, inc := range includes.Content {
			p := inc.Value
			if !filepath.IsAbs(p) {
				p = filepath.Join(filepath.Dir(path), p)
			}
			child, err := l.read(p, path)
			if err != nil {
				return nil, err
			}
			merge(merged, child)
		}
		l.stack = l.stack[:len(l.stack)-1]
	}
	merge(merged, node)
	return merged, nil
}

func sameFile(a, b string) bool {
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// removeKey deletes key from a mapping node and returns its value.
func removeKey(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			v := m.Content[i+1]
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return v
		}
	}
	return nil
}

// merge applies src over dst: mappings merge key by key; anything else
// replaces.
func merge(dst, src *yaml.Node) {
	for i := 0; i+1 < len(src.Content); i += 2 {
		k, v := src.Content[i], src.Content[i+1]
		found := false
		for j := 0; j+1 < len(dst.Content); j += 2 {
			if dst.Content[j].Value != k.Value {
				continue
			}
			found = true
			if dst.Content[j+1].Kind == yaml.MappingNode && v.Kind == yaml.MappingNode {
				merge(dst.Content[j+1], v)
			} else {
				dst.Content[j+1] = v
			}
			break
		}
		if !found {
			dst.Content = append(dst.Content, k, v)
		}
	}
}

// allowPlaintext reads the allow_plaintext_secrets field of a config.
func allowPlaintext(v any) bool {
	rv := reflect.ValueOf(v).Elem()
	for i := range rv.NumField() {
		if yamlName(rv.Type().Field(i)) == plaintextKey {
			return rv.Field(i).Kind() == reflect.Bool && rv.Field(i).Bool()
		}
	}
	return false
}

var refType = reflect.TypeFor[secrets.Ref]()

// walkRefs calls fn for every secret reference in v, with its key path.
func walkRefs(v reflect.Value, path []string, fn func([]string, secrets.Ref)) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			walkRefs(v.Elem(), path, fn)
		}
	case reflect.Struct:
		if v.Type() == refType {
			if r := v.Interface().(secrets.Ref); !r.IsZero() {
				fn(path, r)
			}
			return
		}
		for i := range v.NumField() {
			f := v.Type().Field(i)
			if name := yamlName(f); name != "" && f.IsExported() {
				walkRefs(v.Field(i), append(path[:len(path):len(path)], name), fn)
			}
		}
	case reflect.Map:
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
		for _, k := range keys {
			walkRefs(v.MapIndex(k), append(path[:len(path):len(path)], fmt.Sprint(k)), fn)
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			walkRefs(v.Index(i), append(path[:len(path):len(path)], strconv.Itoa(i)), fn)
		}
	}
}

// yamlName returns a struct field's YAML key, or "" if it has none.
func yamlName(f reflect.StructField) string {
	name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
	if name == "-" {
		return ""
	}
	return name
}

var (
	lineRE     = regexp.MustCompile(`line (\d+): (.*)`)
	unknownRE  = regexp.MustCompile(`field (\S+) not found in type \S+`)
	backtickRE = regexp.MustCompile("`[^`]*`")
	typeRE     = regexp.MustCompile(`cannot unmarshal (.+?) into (\S+)`)
	tagNames   = strings.NewReplacer("!!str", "a string", "!!int", "a number", "!!float", "a number",
		"!!bool", "true or false", "!!seq", "a list", "!!map", "a mapping", "!!null", "an empty value",
		"!!timestamp", "a timestamp", "!!binary", "binary data")
)

// yamlProblem turns a YAML error into a line number and a plain message.
// Values quoted by the YAML library are removed, so a misplaced secret is
// never echoed.
func yamlProblem(err error) (int, string) {
	var lines []string
	var te *yaml.TypeError
	if errors.As(err, &te) {
		lines = te.Errors
	} else {
		lines = []string{strings.TrimPrefix(err.Error(), "yaml: ")}
	}
	line, text := 0, lines[0]
	if m := lineRE.FindStringSubmatch(text); m != nil {
		line, _ = strconv.Atoi(m[1])
		text = m[2]
	}
	if m := unknownRE.FindStringSubmatch(text); m != nil {
		text = fmt.Sprintf("unknown key %q", m[1])
	}
	text = backtickRE.ReplaceAllString(text, "")
	text = tagNames.Replace(text)
	if m := typeRE.FindStringSubmatch(text); m != nil {
		text = fmt.Sprintf("expected %s, got %s", goTypeName(m[2]), strings.TrimSpace(m[1]))
	}
	if len(lines) > 1 {
		text += fmt.Sprintf(" (and %d more problems)", len(lines)-1)
	}
	return line, text
}

// goTypeName describes a Go type from a YAML error in config terms.
func goTypeName(t string) string {
	t = strings.TrimPrefix(t, "*")
	switch {
	case strings.HasPrefix(t, "[]"):
		return "a list"
	case t == "bool":
		return "true or false"
	case t == "string":
		return "a string"
	case strings.HasPrefix(t, "int") || strings.HasPrefix(t, "uint") || strings.HasPrefix(t, "float"):
		return "a number"
	}
	return "a mapping"
}

// invalid is a config problem in file at line (0 when unknown).
func invalid(file string, line int, problem string) error {
	where, next := "config "+file, "correct "+file
	if line > 0 {
		where += fmt.Sprintf(" line %d", line)
		next = fmt.Sprintf("correct line %d of %s", line, file)
	}
	return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDInvalid, Fact: where + ": " + problem, Next: next,
	}}
}

func readError(path, includedFrom string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		fact := "config file " + path + " not found"
		next := "create it, or point to the config with --config or " + EnvConfigPath
		if includedFrom != "" {
			fact = fmt.Sprintf("config file %s, included from %s, not found", path, includedFrom)
			next = "correct the include list in " + includedFrom
		}
		return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDNotFound, Fact: fact, Next: next,
		}}
	}
	cause := "cannot read the file"
	if errors.Is(err, fs.ErrPermission) {
		cause = "permission denied"
	}
	return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
		Level: msg.LevelFailed, ID: IDInvalid, Fact: "config file " + path + " cannot be read", Cause: cause,
		Next: "check that the user pentym runs as can read " + path,
	}}
}

// checkPrivate rejects a file holding plaintext secrets that other users
// can read.
func checkPrivate(file string) error {
	readable, err := readableByOthers(file)
	if err != nil {
		return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDPlaintextReadable,
			Fact:  "cannot check who can read " + file + ", which holds plaintext secrets",
			Cause: err.Error(), Next: privateFix(file),
		}}
	}
	if readable {
		return &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, ID: IDPlaintextReadable,
			Fact:   file + " holds plaintext secrets and other users can read it",
			Impact: "anyone on this machine can read those secrets",
			Next:   privateFix(file),
		}}
	}
	return nil
}
