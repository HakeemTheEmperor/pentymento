// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Toluwalase Akinyemi

package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"

	"github.com/HakeemTheEmperor/pentymento/internal/config"
	"github.com/HakeemTheEmperor/pentymento/internal/engine"
	"github.com/HakeemTheEmperor/pentymento/internal/msg"
)

// Capture timings (PRD 9.1).
const (
	// PartialInterval is how often the current segment is stored while
	// new WAL arrives, so the recovery point stays at seconds.
	PartialInterval = 10 * time.Second
	// StatusInterval is how often the stored position is reported to the
	// server, at the least, so the slot frees WAL.
	StatusInterval = 10 * time.Second
	// receiveWake bounds a wait for the server, so a stop is noticed
	// quickly without closing the connection.
	receiveWake = 500 * time.Millisecond
	// flushTimeout bounds storing the last partial segment on a stop.
	flushTimeout = 20 * time.Second
)

// WALObserver receives WAL as it arrives, before it is stored: the event
// detector (SDD 6.3) plugs in here.
type WALObserver interface {
	WAL(timeline uint32, start pglogrepl.LSN, data []byte)
}

// captureCred is the role capture connects as.
func captureCred(t engine.Target) (config.Credential, error) {
	c := t.Config.Credentials.CaptureCredential()
	if c.User == "" {
		return c, &msg.Error{Code: msg.ExitConfig, Message: msg.Message{
			Level: msg.LevelFailed, Target: t.Name,
			Fact: "no role configured for WAL capture (credentials.replication.user or credentials.backup.user)",
			Next: "set targets." + t.Name + ".credentials.replication.user in the config",
		}}
	}
	return c, nil
}

// slotInfo is a replication slot as the server reports it.
type slotInfo struct {
	exists     bool
	physical   bool
	restartLSN pglogrepl.LSN
	walStatus  string
}

// slot reads the target's slot on a normal connection.
func (e *Engine) slot(ctx context.Context, t engine.Target, cred config.Credential, name string) (slotInfo, error) {
	conn, err := e.connectAs(ctx, t, cred, false)
	if err != nil {
		return slotInfo{}, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := query(ctx, conn, "SELECT slot_type, coalesce(restart_lsn::text, ''), coalesce(wal_status, '') FROM pg_replication_slots WHERE slot_name = "+conninfoValue(name))
	if err != nil {
		return slotInfo{}, fmt.Errorf("read the replication slot %s: %w", name, err)
	}
	if len(rows) == 0 {
		return slotInfo{}, nil
	}
	s := slotInfo{exists: true, physical: rows[0][0] == "physical", walStatus: rows[0][2]}
	if rows[0][1] != "" {
		if s.restartLSN, err = pglogrepl.ParseLSN(rows[0][1]); err != nil {
			return slotInfo{}, err
		}
	}
	return s, nil
}

// DetectGap reports whether the WAL since a checkpoint can still be
// streamed from the target's slot: not if the slot is gone or invalidated,
// or has moved past the checkpoint (WAL removed). The slot is only ever
// moved forward by pentym's acknowledgements, which follow the
// checkpoint, so a slot behind or at the checkpoint holds all that is
// needed.
func (e *Engine) DetectGap(ctx context.Context, t engine.Target, checkpoint engine.Position) (*engine.Gap, error) {
	if checkpoint == "" {
		return nil, nil
	}
	cp, err := parseWALPos(checkpoint)
	if err != nil {
		return nil, err
	}
	cred, err := captureCred(t)
	if err != nil {
		return nil, err
	}
	name := t.Config.SlotName(t.Name)
	s, err := e.slot(ctx, t, cred, name)
	if err != nil {
		return nil, err
	}
	switch {
	case !s.exists:
		return &engine.Gap{From: checkpoint, Reason: "the replication slot " + name + " no longer exists, so the WAL after the checkpoint was not kept"}, nil
	case !s.physical:
		return nil, fmt.Errorf("the replication slot %s is not a physical slot", name)
	case s.walStatus == "lost":
		return &engine.Gap{From: checkpoint, Reason: "the replication slot " + name + " was invalidated (max_slot_wal_keep_size), and the WAL it held was removed"}, nil
	case s.restartLSN > cp.lsn:
		return &engine.Gap{From: checkpoint, To: walPos{tl: cp.tl, lsn: s.restartLSN}.position(),
			Reason: "the server removed WAL after the checkpoint that the replication slot should have kept"}, nil
	}
	return nil, nil
}

// ensureSlot makes sure the target's physical slot exists and is usable,
// creating it if needed (it then holds WAL from now on). An invalidated
// slot holds no WAL, so it is dropped and created again; the gap was
// recorded before. pentym never drops a slot that still holds WAL.
func (e *Engine) ensureSlot(ctx context.Context, t engine.Target, cred config.Credential, repl *pgconn.PgConn) (slotInfo, error) {
	name := t.Config.SlotName(t.Name)
	s, err := e.slot(ctx, t, cred, name)
	if err != nil {
		return s, err
	}
	if s.exists && !s.physical {
		return s, fmt.Errorf("the replication slot %s is a logical slot; set targets.%s.slot to a physical slot's name", name, t.Name)
	}
	if s.exists && s.walStatus == "lost" {
		e.logger().Warn("dropping the invalidated replication slot to create it again", "target", t.Name, "slot", name)
		if err := pglogrepl.DropReplicationSlot(ctx, repl, name, pglogrepl.DropReplicationSlotOptions{}); err != nil {
			return s, fmt.Errorf("drop the invalidated replication slot %s: %w", name, err)
		}
		s.exists = false
	}
	if !s.exists {
		err := repl.Exec(ctx, "CREATE_REPLICATION_SLOT "+name+" PHYSICAL (RESERVE_WAL)").Close()
		var pgErr *pgconn.PgError
		if err != nil && (!errors.As(err, &pgErr) || pgErr.Code != "42710") { // 42710: created meanwhile
			return s, fmt.Errorf("create the replication slot %s: %w", name, err)
		}
		e.logger().Info("created the replication slot", "target", t.Name, "slot", name)
		if s, err = e.slot(ctx, t, cred, name); err != nil {
			return s, err
		}
	}
	return s, nil
}

// walSegmentSize reads the cluster's WAL segment size.
func walSegmentSize(ctx context.Context, repl *pgconn.PgConn) (uint64, error) {
	res, err := repl.Exec(ctx, "SHOW wal_segment_size").ReadAll()
	if err != nil {
		return 0, fmt.Errorf("read wal_segment_size: %w", err)
	}
	if len(res) != 1 || len(res[0].Rows) != 1 {
		return 0, errors.New("read wal_segment_size: no answer")
	}
	size, err := parseSize(string(res[0].Rows[0][0]))
	if err != nil || size == 0 || 0x100000000%size != 0 {
		return 0, fmt.Errorf("wal_segment_size %q is not usable", res[0].Rows[0][0])
	}
	return size, nil
}

// Capture streams WAL from the target's physical replication slot into
// sink until ctx ends (SDD 6.2): from the checkpoint, or from where the
// slot stands when there is none. Each completed segment is stored as it
// fills; the segment in progress is stored every PartialInterval while
// new WAL arrives, and once more when capture stops. Only WAL sink has
// stored is acknowledged to the server, so the slot never frees WAL that
// is not safe. Timeline switches are followed, with their history files.
func (e *Engine) Capture(ctx context.Context, t engine.Target, from engine.Position, sink engine.LogSink) error {
	cred, err := captureCred(t)
	if err != nil {
		return err
	}
	repl, err := e.connectAs(ctx, t, cred, true)
	if err != nil {
		return err
	}
	defer func() { _ = repl.Close(context.WithoutCancel(ctx)) }()
	segSize, err := walSegmentSize(ctx, repl)
	if err != nil {
		return err
	}
	sys, err := pglogrepl.IdentifySystem(ctx, repl)
	if err != nil {
		return fmt.Errorf("identify the system: %w", err)
	}
	slot, err := e.ensureSlot(ctx, t, cred, repl)
	if err != nil {
		return err
	}
	pos := walPos{tl: uint32(sys.Timeline), lsn: slot.restartLSN} // #nosec G115 -- a timeline ID
	if from != "" {
		if pos, err = parseWALPos(from); err != nil {
			return err
		}
		if slot.restartLSN > pos.lsn {
			return fmt.Errorf("%w: the slot starts at %s, after the checkpoint %s", engine.ErrGap, slot.restartLSN, pos.lsn)
		}
	}
	name := t.Config.SlotName(t.Name)
	e.logger().Info("capturing WAL", "target", t.Name, "slot", name, "from", pos.position())
	for {
		next, err := e.stream(ctx, repl, name, segSize, pos, sink)
		if err != nil || next == nil {
			return err
		}
		// The server finished the timeline: keep the new timeline's
		// history, then follow it.
		hist, err := pglogrepl.TimelineHistory(ctx, repl, int32(next.tl)) // #nosec G115 -- a timeline ID
		if err != nil {
			return fmt.Errorf("read the history of timeline %d: %w", next.tl, err)
		}
		if err := sink.Segment(ctx, engine.Segment{
			Kind: "history", Name: hist.FileName, Timeline: strconv.FormatUint(uint64(next.tl), 10), Data: hist.Content,
		}); err != nil {
			return err
		}
		e.logger().Info("following a new timeline", "target", t.Name, "timeline", next.tl, "from", next.lsn)
		pos = *next
	}
}

// stream streams one timeline from pos. It returns the next timeline's
// start when the server finishes this one, or nil when ctx ends.
func (e *Engine) stream(ctx context.Context, repl *pgconn.PgConn, slot string, segSize uint64, pos walPos, sink engine.LogSink) (*walPos, error) {
	asm := newWALAssembler(pos.tl, segSize, pos.lsn)
	stored := pos.lsn // what is safe in the repository
	if err := pglogrepl.StartReplication(ctx, repl, slot, asm.start(), pglogrepl.StartReplicationOptions{
		Timeline: int32(pos.tl), Mode: pglogrepl.PhysicalReplication, // #nosec G115 -- a timeline ID
	}); err != nil {
		return nil, fmt.Errorf("start replication from %s: %w", asm.start(), err)
	}
	tl := strconv.FormatUint(uint64(pos.tl), 10)
	store := func(ctx context.Context, s walSegment) error {
		kind := "wal-partial"
		if s.complete {
			kind = "wal"
		}
		return sink.Segment(ctx, engine.Segment{
			Kind: kind, Name: s.name, Timeline: tl,
			First: engine.Position(s.start.String()), Last: engine.Position(s.end.String()),
			Checkpoint: walPos{tl: pos.tl, lsn: s.end}.position(), Data: s.data,
		})
	}
	status := func(ctx context.Context) error {
		return pglogrepl.SendStandbyStatusUpdate(ctx, repl, pglogrepl.StandbyStatusUpdate{
			WALWritePosition: asm.received(), WALFlushPosition: stored, WALApplyPosition: stored, ClientTime: time.Now(),
		})
	}
	// flush stores the segment in progress beyond what is stored, then
	// reports it.
	flush := func(ctx context.Context) error {
		if p, ok := asm.partial(); ok && p.end > stored {
			if err := store(ctx, p); err != nil {
				return err
			}
			stored = p.end
		}
		return status(ctx)
	}
	nextPartial := time.Now().Add(PartialInterval)
	nextStatus := time.Now()
	for {
		if ctx.Err() != nil {
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), flushTimeout)
			err := flush(fctx)
			cancel()
			return nil, err
		}
		now := time.Now()
		if !now.Before(nextPartial) {
			if p, ok := asm.partial(); ok && p.end > stored {
				if err := store(ctx, p); err != nil {
					return nil, err
				}
				stored = p.end
				nextStatus = now
			}
			nextPartial = now.Add(PartialInterval)
		}
		if !now.Before(nextStatus) {
			if err := status(ctx); err != nil {
				return nil, fmt.Errorf("report the stored position: %w", err)
			}
			nextStatus = now.Add(StatusInterval)
		}
		wake := now.Add(receiveWake)
		for _, t := range []time.Time{nextPartial, nextStatus} {
			if t.Before(wake) {
				wake = t
			}
		}
		// A deadline that is not ctx's: a timeout leaves the connection
		// usable, a cancellation would close it.
		rctx, cancel := context.WithDeadline(context.WithoutCancel(ctx), wake)
		raw, err := repl.ReceiveMessage(rctx)
		cancel()
		if err != nil {
			if pgconn.Timeout(err) {
				continue
			}
			return nil, fmt.Errorf("receive WAL: %w", err)
		}
		switch m := raw.(type) {
		case *pgproto3.CopyData:
			if len(m.Data) == 0 {
				continue
			}
			switch m.Data[0] {
			case pglogrepl.PrimaryKeepaliveMessageByteID:
				ka, err := pglogrepl.ParsePrimaryKeepaliveMessage(m.Data[1:])
				if err != nil {
					return nil, err
				}
				if ka.ReplyRequested {
					nextStatus = time.Now()
				}
			case pglogrepl.XLogDataByteID:
				xld, err := pglogrepl.ParseXLogData(m.Data[1:])
				if err != nil {
					return nil, err
				}
				if e.Observer != nil {
					e.Observer.WAL(pos.tl, xld.WALStart, xld.WALData)
				}
				done, err := asm.add(xld.WALStart, xld.WALData)
				if err != nil {
					return nil, err
				}
				for _, s := range done {
					if err := store(ctx, s); err != nil {
						return nil, err
					}
					stored = s.end
					nextStatus = time.Now()
				}
			}
		case *pgproto3.CopyDone:
			// The timeline ended: store what remains of it, then ask where
			// the next one starts.
			if err := flush(ctx); err != nil {
				return nil, err
			}
			cdr, err := pglogrepl.SendStandbyCopyDone(ctx, repl)
			if err != nil {
				return nil, fmt.Errorf("end the timeline: %w", err)
			}
			if cdr == nil {
				return nil, errors.New("the server ended the WAL stream without naming the next timeline")
			}
			return &walPos{tl: uint32(cdr.Timeline), lsn: cdr.LSN}, nil // #nosec G115 -- a timeline ID
		case *pgproto3.ErrorResponse:
			return nil, pgconn.ErrorResponseToPgError(m)
		}
	}
}

// LogHealth reports the server's newest WAL position and what the slot
// holds back, for slot monitoring (PRD 7.3).
func (e *Engine) LogHealth(ctx context.Context, t engine.Target) (*engine.LogHealth, error) {
	cred, err := captureCred(t)
	if err != nil {
		return nil, err
	}
	repl, err := e.connectAs(ctx, t, cred, true)
	if err != nil {
		return nil, err
	}
	sys, err := pglogrepl.IdentifySystem(ctx, repl)
	_ = repl.Close(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("identify the system: %w", err)
	}
	h := &engine.LogHealth{Head: walPos{tl: uint32(sys.Timeline), lsn: sys.XLogPos}.position()} // #nosec G115 -- a timeline ID
	conn, err := e.connectAs(ctx, t, cred, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := query(ctx, conn, `SELECT coalesce(pg_wal_lsn_diff(`+conninfoValue(sys.XLogPos.String())+`::pg_lsn, s.restart_lsn)::bigint, 0),
		coalesce(s.wal_status, ''), (SELECT setting::bigint FROM pg_settings WHERE name = 'max_slot_wal_keep_size')
		FROM (SELECT 1) AS one LEFT JOIN pg_replication_slots s ON s.slot_name = `+conninfoValue(t.Config.SlotName(t.Name)))
	if err != nil {
		return nil, fmt.Errorf("read the replication slot: %w", err)
	}
	h.RetainedBytes, _ = strconv.ParseInt(rows[0][0], 10, 64)
	h.Lost = rows[0][1] == "lost"
	if mb, _ := strconv.ParseInt(rows[0][2], 10, 64); mb > 0 {
		h.RetainedLimit = mb << 20
	}
	return h, nil
}
