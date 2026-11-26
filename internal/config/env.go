// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package config

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// envPrefix starts every environment override.
const envPrefix = "PENTYM_"

// reservedEnv are PENTYM_ variables that are not config overrides.
var reservedEnv = []string{EnvConfigPath, "PENTYM_TEST_"}

var unmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()

// applyEnv applies PENTYM_<KEY>__<KEY>… overrides from environ (as from
// os.Environ) to the config tree. Names match keys case-insensitively, with
// _ matching -. It returns the overridden key paths, mapped to the
// variable that set each.
func applyEnv(root *yaml.Node, t reflect.Type, environ []string) (map[string]string, error) {
	vars := append([]string(nil), environ...)
	sort.Strings(vars)
	set := map[string]string{}
	for _, kv := range vars {
		name, value, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(strings.ToUpper(name), envPrefix) || isReserved(name) {
			continue
		}
		path, err := resolveEnvPath(root, t, strings.Split(name[len(envPrefix):], "__"))
		if err != nil {
			return nil, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
				Level: msg.LevelFailed, ID: IDInvalid,
				Fact: "environment variable " + name + ": " + err.Error(),
				Next: "correct or unset " + name,
			}}
		}
		setPath(root, path, value)
		set[strings.Join(path, ".")] = name
	}
	return set, nil
}

func isReserved(name string) bool {
	upper := strings.ToUpper(name)
	for _, r := range reservedEnv {
		if upper == r || (strings.HasSuffix(r, "_") && strings.HasPrefix(upper, r)) {
			return true
		}
	}
	return false
}

// normalize folds case and treats - as _.
func normalize(s string) string { return strings.ReplaceAll(strings.ToLower(s), "-", "_") }

// resolveEnvPath maps name segments to config keys, following the schema
// type t and, for maps such as targets, the keys present in the tree.
func resolveEnvPath(root *yaml.Node, t reflect.Type, segs []string) ([]string, error) {
	var path []string
	node := root
	for _, seg := range segs {
		if seg == "" {
			return nil, fmt.Errorf("empty key in the name; separate keys with a double underscore")
		}
		for t.Kind() == reflect.Pointer {
			t = t.Elem()
		}
		where := "the config"
		if len(path) > 0 {
			where = strings.Join(path, ".")
		}
		var key string
		switch {
		case isLeaf(t):
			return nil, fmt.Errorf("%s has no keys below it", where)
		case t.Kind() == reflect.Struct:
			for i := range t.NumField() {
				f := t.Field(i)
				if name := yamlName(f); name != "" && f.IsExported() && normalize(name) == normalize(seg) {
					key, t = name, f.Type
					break
				}
			}
			if key == "" {
				return nil, fmt.Errorf("%s has no key matching %s", where, seg)
			}
		case t.Kind() == reflect.Map && t.Key().Kind() == reflect.String:
			var matches []string
			for _, k := range mapKeys(node) {
				if normalize(k) == normalize(seg) {
					matches = append(matches, k)
				}
			}
			switch len(matches) {
			case 0:
				return nil, fmt.Errorf("%s has no entry matching %s", where, seg)
			case 1:
				key, t = matches[0], t.Elem()
			default:
				return nil, fmt.Errorf("%s matches several entries in %s: %s", seg, where, strings.Join(matches, ", "))
			}
		default:
			return nil, fmt.Errorf("%s cannot be set from the environment", where)
		}
		path = append(path, key)
		node = child(node, key)
	}
	for t.Kind() == reflect.Pointer { // an optional value, such as agent.write_only
		t = t.Elem()
	}
	if !isLeaf(t) {
		return nil, fmt.Errorf("%s is not a single value, so it cannot be set from the environment", strings.Join(path, "."))
	}
	return path, nil
}

// isLeaf reports whether t holds a single scalar value.
func isLeaf(t reflect.Type) bool {
	if reflect.PointerTo(t).Implements(unmarshalerType) {
		return true
	}
	switch t.Kind() {
	case reflect.String, reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	}
	return false
}

func mapKeys(m *yaml.Node) []string {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	var keys []string
	for i := 0; i+1 < len(m.Content); i += 2 {
		keys = append(keys, m.Content[i].Value)
	}
	return keys
}

func child(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setPath sets the scalar at path, creating mappings along the way.
func setPath(root *yaml.Node, path []string, value string) {
	node := root
	for i, key := range path {
		next := child(node, key)
		last := i == len(path)-1
		if next == nil || (!last && next.Kind != yaml.MappingNode) {
			next = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			if last {
				next = &yaml.Node{}
			}
			replaceOrAppend(node, key, next)
		}
		if last {
			*next = yaml.Node{Kind: yaml.ScalarNode, Value: value}
		}
		node = next
	}
}

func replaceOrAppend(m *yaml.Node, key string, v *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = v
			return
		}
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, v)
}
