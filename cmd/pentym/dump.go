// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package main

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/HakeemTheEmperor/pentymento/internal/backend"
	"github.com/HakeemTheEmperor/pentymento/internal/cliout"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
	"github.com/HakeemTheEmperor/pentymento/internal/repo"
)

// snapshotIDRE is the form of a snapshot ID.
var snapshotIDRE = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Dump formats.
const (
	// dumpTar is a tar holding the snapshot's archives (Postgres).
	dumpTar = "tar"
	// dumpStream is the snapshot's one stream as stored (MongoDB's
	// mongodump archive).
	dumpStream = "stream"
)

// dumpFormat chooses how a snapshot is exported: a snapshot of tar
// archives, or of several streams, becomes one tar holding them, so its
// layout is the same whatever the backup method; a single other stream is
// written as it is.
func dumpFormat(s *repo.Snapshot) string {
	streams := s.Streams()
	if len(streams) > 1 || strings.HasSuffix(streams[0].Name, ".tar") {
		return dumpTar
	}
	return dumpStream
}

func (a *app) dumpCommand() *cobra.Command {
	var output, slot string
	c := &cobra.Command{
		Use:   "dump <snapshot>",
		Short: "Export a snapshot as a plain file",
		Long: "Export a snapshot as a plain file, decrypted and reassembled, that the database's own tools restore without pentym. " +
			"A Postgres snapshot becomes one tar holding its archives (base.tar, a tar per tablespace, and backup_manifest), " +
			"the layout pg_basebackup --format=tar writes; a MongoDB snapshot becomes its mongodump archive, which " +
			"mongorestore --archive reads.\n\n" +
			"The dump goes to --output, a new file, or to stdout when stdout is not a terminal. It streams with constant memory, " +
			"checks every chunk, and needs a full key slot. A failed dump leaves no file behind.",
		Example: example(
			"Export a snapshot to a file", "pentym dump 3f9c2a7d18e04b6f9a1c5d2e7b8f0a14 --output prod-pg.tar",
			"Restore a MongoDB snapshot with mongorestore", "pentym dump 3f9c2a7d18e04b6f9a1c5d2e7b8f0a14 | mongorestore --archive --oplogReplay"),
		Args: args("<snapshot>"),
		RunE: func(cmd *cobra.Command, argv []string) error {
			if !snapshotIDRE.MatchString(argv[0]) {
				return newUsageError(cmd, "a snapshot ID is 32 hexadecimal characters, as pentym list shows")
			}
			if output == "" {
				if a.opts.json {
					return newUsageError(cmd, "--json needs --output: without it the dump itself goes to stdout")
				}
				if a.printer.OutTerminal {
					return newUsageError(cmd, "stdout is a terminal; write the dump with --output <file> or redirect stdout")
				}
			}
			return a.runDump(cmd.Context(), argv[0], output, slot)
		},
	}
	c.Flags().StringVar(&output, "output", "", "write the dump to this new file instead of stdout")
	addSlotFlag(c, &slot)
	return c
}

// dumpResult is what dump --json reports.
type dumpResult struct {
	Snapshot string   `json:"snapshot"`
	Output   string   `json:"output"`
	Format   string   `json:"format"`
	Bytes    int64    `json:"bytes"`
	Streams  []string `json:"streams"`
}

func (a *app) runDump(ctx context.Context, id, output, slot string) error {
	cfg, r, err := a.openRepo(ctx)
	if err != nil {
		return err
	}
	if _, err := a.unlock(ctx, cfg, r, repo.UnlockOptions{SlotID: slot, NeedFull: true}); err != nil {
		return err
	}
	s, err := r.LoadSnapshot(ctx, id)
	if err != nil {
		if errors.Is(err, backend.ErrNotFound) {
			return &msg.Error{Code: msg.ExitNotFound, Err: err, Message: msg.Message{
				Level: msg.LevelFailed, Fact: "no snapshot " + id + " in the repository", Next: "pentym list",
			}}
		}
		return err
	}
	idx, err := r.LoadIndex(ctx)
	if err != nil {
		return err
	}
	op := beginOp(r, "dump", cfg.Repository.Key)
	res, err := a.writeDump(ctx, r, idx, s, output)
	if err := finishOp(ctx, op, err); err != nil {
		return err
	}
	if a.opts.json {
		return a.printer.WriteJSON(res)
	}
	where := res.Output
	if where == "" {
		where = "stdout"
	}
	a.printer.Message(msg.Message{Level: msg.LevelOK, Fact: fmt.Sprintf("dumped snapshot %s (%s, %s) to %s",
		s.ID, describeFormat(res.Format), cliout.Bytes(res.Bytes), where)})
	return nil
}

func describeFormat(f string) string {
	if f == dumpTar {
		return "a tar of its archives"
	}
	return "its archive as stored"
}

// countingWriter counts what passes through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// writeDump writes the snapshot to output, or stdout, and removes a file
// it could not finish.
func (a *app) writeDump(ctx context.Context, r *repo.Repository, idx *repo.Index, s *repo.Snapshot, output string) (_ *dumpResult, err error) {
	res := &dumpResult{Snapshot: s.ID, Output: output, Format: dumpFormat(s)}
	for _, p := range s.Streams() {
		res.Streams = append(res.Streams, p.Name)
	}
	dst := a.printer.Stdout
	if output != "" {
		var f *os.File
		f, err = os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // #nosec G304 -- the operator's chosen output file
		if errors.Is(err, os.ErrExist) {
			return nil, &msg.Error{Code: msg.ExitUsage, Err: err, Message: msg.Message{
				Level: msg.LevelFailed, Fact: output + " already exists", Next: "choose a new file for --output",
			}}
		}
		if err != nil {
			return nil, err
		}
		defer func() {
			if err == nil {
				err = f.Sync()
			}
			if cerr := f.Close(); err == nil && cerr != nil {
				err = cerr
			}
			if err != nil {
				_ = os.Remove(output)
			}
		}()
		dst = f
	}
	progress, err := a.progress("dump "+s.ID, s.TotalSize())
	if err != nil {
		return nil, err
	}
	progress.Start()
	defer progress.Done()
	cw := &countingWriter{w: dst}
	out := io.Writer(cw)
	streamTo := func(p repo.Part, w io.Writer) error {
		return r.ReadPart(ctx, idx, s, p.Name, &progressWriter{w: w, add: progress.Add})
	}
	if res.Format == dumpStream {
		if err := streamTo(s.Streams()[0], out); err != nil {
			return nil, err
		}
	} else {
		tw := tar.NewWriter(out)
		for _, p := range s.Streams() {
			name := p.Name
			if name == "" {
				name = "stream"
			}
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: name, Size: p.Size, Mode: 0o600, ModTime: s.End, Format: tar.FormatPAX,
			}); err != nil {
				return nil, err
			}
			if err := streamTo(p, tw); err != nil {
				return nil, err
			}
		}
		if err := tw.Close(); err != nil {
			return nil, err
		}
	}
	res.Bytes = cw.n
	return res, nil
}

// progressWriter reports bytes written.
type progressWriter struct {
	w   io.Writer
	add func(int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.add(int64(n))
	return n, err
}
