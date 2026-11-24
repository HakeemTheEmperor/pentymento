// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strconv"
)

var (
	idRE   = regexp.MustCompile(`^PTM-[1-9][0-9]{3}$`)
	nameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	docRE  = regexp.MustCompile(`^(PTM-[0-9]+)\.md$`)
)

// entry is a registry entry as written in the source.
type entry struct {
	ID      string
	Name    string
	Retired bool
}

// parseRegistry reads the entries from the source of internal/msg's
// registry file: the elements of the "entries" slice literal. Each element
// must give ID and Name as string literals.
func parseRegistry(src []byte) ([]entry, error) {
	f, err := parser.ParseFile(token.NewFileSet(), "registry.go", src, 0)
	if err != nil {
		return nil, err
	}
	var lit *ast.CompositeLit
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, s := range gd.Specs {
			vs := s.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name == "entries" && i < len(vs.Values) {
					lit, _ = vs.Values[i].(*ast.CompositeLit)
				}
			}
		}
	}
	if lit == nil {
		return nil, fmt.Errorf("no entries slice literal found")
	}
	var out []entry
	for i, elt := range lit.Elts {
		cl, ok := elt.(*ast.CompositeLit)
		if !ok {
			return nil, fmt.Errorf("entry %d is not a composite literal", i+1)
		}
		var e entry
		for _, f := range cl.Elts {
			kv, ok := f.(*ast.KeyValueExpr)
			if !ok {
				return nil, fmt.Errorf("entry %d must use field names", i+1)
			}
			key, _ := kv.Key.(*ast.Ident)
			if key == nil {
				return nil, fmt.Errorf("entry %d has an unexpected field", i+1)
			}
			switch key.Name {
			case "ID", "Name":
				bl, ok := kv.Value.(*ast.BasicLit)
				if !ok || bl.Kind != token.STRING {
					return nil, fmt.Errorf("entry %d: %s must be a string literal", i+1, key.Name)
				}
				v, err := strconv.Unquote(bl.Value)
				if err != nil {
					return nil, fmt.Errorf("entry %d: %s: %w", i+1, key.Name, err)
				}
				if key.Name == "ID" {
					e.ID = v
				} else {
					e.Name = v
				}
			case "Retired":
				id, ok := kv.Value.(*ast.Ident)
				if !ok || (id.Name != "true" && id.Name != "false") {
					return nil, fmt.Errorf("entry %d: Retired must be true or false", i+1)
				}
				e.Retired = id.Name == "true"
			default:
				return nil, fmt.Errorf("entry %d has an unknown field %s", i+1, key.Name)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// checkRegistry checks the entries on their own and against the docs
// directory listing (file names in docs/errors).
func checkRegistry(entries []entry, docFiles []string) []string {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	ids := map[string]bool{}
	names := map[string]string{}
	docs := map[string]bool{}
	for _, f := range docFiles {
		if m := docRE.FindStringSubmatch(f); m != nil {
			docs[m[1]] = true
		}
	}
	for _, e := range entries {
		if !idRE.MatchString(e.ID) {
			add("%q is not a valid message ID (PTM- and four digits, 1000 to 9999)", e.ID)
		}
		if ids[e.ID] {
			add("%s appears more than once", e.ID)
		}
		ids[e.ID] = true
		if !nameRE.MatchString(e.Name) {
			add("%s: name %q must be lowercase kebab-case", e.ID, e.Name)
		}
		if other, ok := names[e.Name]; ok {
			add("%s: name %q is already used by %s", e.ID, e.Name, other)
		}
		names[e.Name] = e.ID
		if !docs[e.ID] {
			add("%s has no troubleshooting entry at docs/errors/%s.md", e.ID, e.ID)
		}
	}
	for id := range docs {
		if !ids[id] {
			add("docs/errors/%s.md has no entry in the registry", id)
		}
	}
	return problems
}

// compareRegistries checks that head keeps every ID in base with the same
// name, and never un-retires an ID.
func compareRegistries(base, head []entry) []string {
	var problems []string
	byID := map[string]entry{}
	for _, e := range head {
		byID[e.ID] = e
	}
	for _, b := range base {
		h, ok := byID[b.ID]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("%s was removed; mark it Retired instead, so it is never reused", b.ID))
		case h.Name != b.Name:
			problems = append(problems, fmt.Sprintf("%s was renamed from %q to %q; an ID never changes meaning", b.ID, b.Name, h.Name))
		case b.Retired && !h.Retired:
			problems = append(problems, fmt.Sprintf("%s was retired and cannot be reused", b.ID))
		}
	}
	return problems
}
