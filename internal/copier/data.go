package copier

import (
	"context"
	"fmt"
	"strings"

	"github.com/shhac/agent-sql/internal/driver"
	"github.com/shhac/agent-sql/internal/schemadiff"
)

// copyTable runs one table's data phase inside one target transaction, so a
// failed table rolls back cleanly. (For replace, the target was already
// emptied by clearTables.) AUTO_INCREMENT is realigned afterwards.
func (p *Plan) copyTable(ctx context.Context, st *schemadiff.Table, r *TableResult) error {
	if p.opts.DryRun {
		return p.planTable(ctx, st, r)
	}
	cols := st.CopyColumns()
	req := driver.ReadRequest{Table: st.Name, Columns: names(cols), Where: p.opts.Where[st.Name]}

	if err := p.writer.Begin(ctx); err != nil {
		return sideError("target", err)
	}
	var err error
	switch p.opts.Data {
	case DataReplace, DataAppend:
		err = p.stream(ctx, req, cols, r, func(batch [][][]byte) error {
			n, err := p.writer.Insert(ctx, st.Name, cols, batch)
			r.Inserted += n
			return err
		})
	case DataUpsert:
		err = p.merge(ctx, st, req, cols, r, mergeWrites)
		r.Upserted, r.Inserted, r.Updated = r.Inserted+r.Updated, 0, 0
	case DataSync:
		if !p.opts.NoDelete && !p.deletesFirst() {
			// Target-only rows go in a pass of their own, before any row is
			// written, so the unique values they hold are free for the rows
			// that replace them; and the merge orders keys by bytes while
			// the target may match them by collation (source 'Alice' would
			// collide with target-only 'alice'). Under deletesFirst they
			// already went in deleteMissing.
			var scratch TableResult
			err = p.merge(ctx, st, req, cols, &scratch, mergeDeletes)
			r.Deleted += scratch.Deleted
		}
		if err == nil {
			err = p.merge(ctx, st, req, cols, r, mergeWrites)
		}
	}
	if err != nil {
		_ = p.writer.Rollback(ctx)
		return err
	}
	if err := p.writer.Commit(ctx); err != nil {
		return sideError("target", err)
	}
	return p.realignAutoIncrement(ctx, st)
}

// realignAutoIncrement raises the target's AUTO_INCREMENT to the source's,
// so new target inserts never collide with copied ids.
func (p *Plan) realignAutoIncrement(ctx context.Context, st *schemadiff.Table) error {
	if st.AutoIncrement == 0 {
		return nil
	}
	cur, err := p.writer.AutoIncrement(ctx, st.Name)
	if err != nil {
		return sideError("target", err)
	}
	if st.AutoIncrement > cur {
		if err := p.writer.RaiseAutoIncrement(ctx, st.Name, st.AutoIncrement); err != nil {
			return sideError("target", err)
		}
	}
	return nil
}

func names(cols []*schemadiff.Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

// stream reads the source table and hands it to flush in batches bounded by
// row count and payload size.
func (p *Plan) stream(ctx context.Context, req driver.ReadRequest, cols []*schemadiff.Column, r *TableResult, flush func([][][]byte) error) error {
	rows, err := p.reader.Read(ctx, req)
	if err != nil {
		return sideError("source", err)
	}
	defer func() { _ = rows.Close() }()
	b := p.newBatcher(len(cols), flush)
	for rows.Next() {
		r.RowsRead++
		p.progress(req.Table, r.RowsRead)
		if err := b.add(rows.Values()); err != nil {
			return sideError("target", err)
		}
	}
	if err := rows.Err(); err != nil {
		return sideError("source", err)
	}
	if err := b.flush(); err != nil {
		return sideError("target", err)
	}
	return nil
}

func (p *Plan) progress(table string, n int64) {
	if p.opts.OnProgress != nil && n%progressEvery == 0 {
		p.opts.OnProgress(table, n)
	}
}

type batcher struct {
	max   int
	rows  [][][]byte
	bytes int
	fn    func([][][]byte) error
}

func (p *Plan) newBatcher(ncols int, fn func([][][]byte) error) *batcher {
	return &batcher{max: p.batchRows(ncols), fn: fn}
}

func (b *batcher) add(row [][]byte) error {
	b.rows = append(b.rows, row)
	for _, v := range row {
		b.bytes += len(v)
	}
	if len(b.rows) >= b.max || b.bytes >= maxBatchBytes {
		return b.flush()
	}
	return nil
}

func (b *batcher) flush() error {
	if len(b.rows) == 0 {
		return nil
	}
	err := b.fn(b.rows)
	b.rows = b.rows[:0] // callbacks write synchronously and keep no reference
	b.bytes = 0
	return err
}

// mergeOps selects what a merge writes.
type mergeOps int

const (
	// mergeWrites inserts source-only rows and updates changed ones. The
	// target is read unfiltered: --where picks source rows, and the target
	// row a source row updates may itself fall outside the condition.
	mergeWrites mergeOps = 1 << iota
	// mergeDeletes deletes target-only rows (unless --no-delete), among the
	// target rows --where selects.
	mergeDeletes
	// mergeKeepReferenced leaves target-only rows a foreign key still
	// references (see driver.ReferencedKeeper), counting only real deletes.
	mergeKeepReferenced
	// mergeCountOnly counts what the other ops would do, writing nothing.
	mergeCountOnly
)

// deletesFirst reports whether sync must delete target-only rows in
// separate children-first passes: when foreign key checks stay on, a parent
// row can only go once its children have. One pass runs before the data
// pass (so a deleted row's unique values are free for the rows that replace
// it) and keeps rows still referenced; the other runs after it, once
// children that moved to another parent no longer point at them.
func (p *Plan) deletesFirst() bool {
	return p.opts.Data == DataSync && !p.opts.NoDelete && !p.opts.DryRun && !p.writer.ChecksOff()
}

// deleteMissing runs sync's deletes for every table not in skip, children
// first, each table in its own transaction. Only used when deletesFirst;
// keepReferenced leaves rows a foreign key still references.
func (p *Plan) deleteMissing(ctx context.Context, rc *Receipt, result func(string) *TableResult, failed map[string]bool, keepReferenced bool) error {
	ops := mergeDeletes
	if keepReferenced {
		ops |= mergeKeepReferenced
	}
	order := p.source.TablesInOrder()
	for i := len(order) - 1; i >= 0; i-- {
		st := order[i]
		if failed[st.Name] || (!keepReferenced && !p.keptReferenced[st.Name]) {
			// The data pass only writes source rows, so a table whose first
			// pass kept nothing has no target-only rows left.
			continue
		}
		r := result(st.Name)
		cols := st.CopyColumns()
		req := driver.ReadRequest{Table: st.Name, Columns: names(cols), Where: p.opts.Where[st.Name]}
		err := p.writer.Begin(ctx)
		if err == nil {
			// Counts only the deletes; the row reads are counted by the
			// main pass.
			var scratch TableResult
			err = p.merge(ctx, st, req, cols, &scratch, ops)
			r.Deleted += scratch.Deleted
			if err != nil {
				_ = p.writer.Rollback(ctx)
			} else {
				err = p.writer.Commit(ctx)
			}
		}
		if err != nil {
			if err := p.tableFailed(rc, r, failed, sideError("target", err)); err != nil {
				return err
			}
		}
	}
	return nil
}

// merge is the data synchronization: both sides are read in key order and
// merge-joined, so memory is bounded by the batch size. Source-only rows are
// inserted and rows whose non-key columns differ byte-for-byte are updated by
// key, as plain INSERT and UPDATE: a write that collides with another row on
// a secondary unique index fails (and rolls the table back) rather than
// landing on that row. Target-only rows are deleted unless --no-delete.
// ops selects what is done (see mergeOps); writes and deletes read the target
// differently under --where, so they share a pass only without one.
func (p *Plan) merge(ctx context.Context, st *schemadiff.Table, req driver.ReadRequest, cols []*schemadiff.Column, r *TableResult, ops mergeOps) error {
	keyCols := st.RowKey()
	writes, dels, write := ops&mergeWrites != 0, ops&mergeDeletes != 0 && !p.opts.NoDelete, ops&mergeCountOnly == 0
	if writes && dels && strings.TrimSpace(req.Where) != "" {
		return fmt.Errorf("merge of %s: writes and deletes need separate passes under --where", st.Name)
	}
	if !writes {
		// A delete-only pass compares keys and nothing else.
		cols = keyCols
		req.Columns = names(keyCols)
	}
	kc := keyCmp{}
	pos := map[string]int{}
	for i, c := range cols {
		pos[c.Name] = i
	}
	for _, k := range keyCols {
		kind := schemadiff.KeyKindOf(k)
		i, ok := pos[k.Name]
		if !ok {
			return fmt.Errorf("key column %s.%s is not among the copied columns", st.Name, k.Name)
		}
		kc.idx = append(kc.idx, i)
		kc.kinds = append(kc.kinds, kind)
		req.OrderBy = append(req.OrderBy, driver.KeyColumn{Name: k.Name, Kind: kind})
	}

	src, err := p.reader.Read(ctx, req)
	if err != nil {
		return sideError("source", err)
	}
	defer func() { _ = src.Close() }()
	tgtReq := req
	if writes {
		tgtReq.Where = ""
	}
	tgt, err := p.targetRows.Read(ctx, tgtReq)
	if err != nil {
		return sideError("target", err)
	}
	defer func() { _ = tgt.Close() }()

	deletes := p.newBatcher(len(keyCols), func(batch [][][]byte) error {
		if !write {
			return nil
		}
		if ops&mergeKeepReferenced != 0 {
			if rk, ok := p.writer.(driver.ReferencedKeeper); ok {
				_, kept, err := rk.DeleteUnreferenced(ctx, st.Name, keyCols, batch)
				r.Deleted -= int64(len(kept))
				if len(kept) > 0 {
					p.keptReferenced[st.Name] = true
				}
				return err
			}
		}
		_, err := p.writer.Delete(ctx, st.Name, keyCols, batch)
		return err
	})
	updates := p.newBatcher(len(cols), func(batch [][][]byte) error {
		if !write {
			return nil
		}
		_, err := p.writer.Update(ctx, st.Name, cols, keyCols, batch)
		return err
	})
	inserts := p.newBatcher(len(cols), func(batch [][][]byte) error {
		if !write {
			return nil
		}
		// Updates queued so far go first, so a unique value a row moves
		// away from is free for the inserts.
		if err := updates.flush(); err != nil {
			return err
		}
		_, err := p.writer.Insert(ctx, st.Name, cols, batch)
		return err
	})
	keyOf := func(row [][]byte) [][]byte {
		out := make([][]byte, len(kc.idx))
		for i, j := range kc.idx {
			out[i] = row[j]
		}
		return out
	}

	var s, t [][]byte
	advanceS := func() error {
		s = nil
		if src.Next() {
			s = src.Values()
			r.RowsRead++
			p.progress(st.Name, r.RowsRead)
			return nil
		}
		return src.Err()
	}
	advanceT := func() error {
		t = nil
		if tgt.Next() {
			t = tgt.Values()
			return nil
		}
		return tgt.Err()
	}
	if err := advanceS(); err != nil {
		return sideError("source", err)
	}
	if err := advanceT(); err != nil {
		return sideError("target", err)
	}
	for s != nil || t != nil {
		c := 0
		switch {
		case t == nil:
			c = -1
		case s == nil:
			c = 1
		default:
			c = kc.compare(s, t)
		}
		switch {
		case c < 0:
			if writes {
				r.Inserted++
				if err := inserts.add(s); err != nil {
					return sideError("target", err)
				}
			}
			if err := advanceS(); err != nil {
				return sideError("source", err)
			}
		case c > 0:
			if dels {
				r.Deleted++
				if err := deletes.add(keyOf(t)); err != nil {
					return sideError("target", err)
				}
			}
			if err := advanceT(); err != nil {
				return sideError("target", err)
			}
		default:
			if writes && !sameRow(s, t) {
				r.Updated++
				if err := updates.add(s); err != nil {
					return sideError("target", err)
				}
			}
			if err := advanceS(); err != nil {
				return sideError("source", err)
			}
			if err := advanceT(); err != nil {
				return sideError("target", err)
			}
		}
	}
	if err := deletes.flush(); err != nil {
		return sideError("target", err)
	}
	if err := updates.flush(); err != nil {
		return sideError("target", err)
	}
	if err := inserts.flush(); err != nil {
		return sideError("target", err)
	}
	return nil
}

// planTable counts what the data phase would do, without writing. Tables
// the structure phase creates or recreates count every source row as an
// insert; sync and upsert against an existing table merge-count over the
// columns both sides share, unless the target lacks a key column (the
// structure phase adds it), when no target row matches and every source row
// counts as an insert.
func (p *Plan) planTable(ctx context.Context, st *schemadiff.Table, r *TableResult) error {
	where := p.opts.Where[st.Name]
	tt := p.target.Table(st.Name)
	fresh := tt == nil || p.opts.Structure == schemadiff.StructureRecreate
	if (p.opts.Data == DataUpsert || p.opts.Data == DataSync) && !fresh && hasColumns(tt, st.RowKey()) {
		// The merge counts both sides itself; no COUNT(*) needed.
		var shared []*schemadiff.Column
		for _, c := range st.CopyColumns() {
			if tc := tt.Column(c.Name); tc != nil && !tc.IsGenerated() {
				shared = append(shared, c)
			}
		}
		req := driver.ReadRequest{Table: st.Name, Columns: names(shared), Where: where}
		// The same passes the real run makes (see copyTable), folded into
		// one when no --where sets the two apart.
		ops := mergeWrites | mergeCountOnly
		if p.opts.Data == DataSync {
			if strings.TrimSpace(where) == "" {
				ops |= mergeDeletes
			} else {
				var scratch TableResult
				if err := p.merge(ctx, st, req, shared, &scratch, mergeDeletes|mergeCountOnly); err != nil {
					return err
				}
				r.Deleted += scratch.Deleted
			}
		}
		if err := p.merge(ctx, st, req, shared, r, ops); err != nil {
			return err
		}
		if p.opts.Data == DataUpsert {
			// As copyTable reports it.
			r.Upserted, r.Inserted, r.Updated = r.Inserted+r.Updated, 0, 0
		}
		return nil
	}

	srcCount, err := p.reader.Count(ctx, st.Name, where)
	if err != nil {
		return sideError("source", err)
	}
	existing := int64(0)
	if tt != nil {
		if existing, err = p.targetRows.Count(ctx, st.Name, ""); err != nil {
			return sideError("target", err)
		}
	}
	r.RowsRead, r.Inserted = srcCount, srcCount
	switch p.opts.Data {
	case DataReplace:
		r.Deleted = existing
		r.Cleared = r.Cleared || tt != nil
	case DataAppend:
		if p.opts.Structure == schemadiff.StructureRecreate {
			r.Deleted = existing
		}
	case DataUpsert, DataSync:
		switch {
		case fresh && tt != nil:
			r.Deleted = existing // dropped by recreate
		case tt != nil && p.opts.Data == DataSync && !p.opts.NoDelete:
			// The key is new on the target: the structure phase adds it, so
			// no existing row carries a source key and sync deletes every
			// row it may touch.
			if r.Deleted, err = p.countTargetOnly(ctx, st, tt, where, existing); err != nil {
				return sideError("target", err)
			}
		}
		if p.opts.Data == DataUpsert {
			r.Upserted, r.Inserted = srcCount, 0
		}
	}
	return nil
}

// countTargetOnly counts the target rows a sync may delete when none of them
// can match a source key: those matching --where, or all of them (existing)
// when the condition may name a column the structure phase has yet to add
// (a failed read would abort a PostgreSQL snapshot, so it is not tried).
func (p *Plan) countTargetOnly(ctx context.Context, st, tt *schemadiff.Table, where string, existing int64) (int64, error) {
	if strings.TrimSpace(where) == "" {
		return existing, nil
	}
	lower := strings.ToLower(where)
	for _, c := range st.Columns {
		if tt.Column(c.Name) == nil && strings.Contains(lower, strings.ToLower(c.Name)) {
			return existing, nil
		}
	}
	return p.targetRows.Count(ctx, st.Name, where)
}

// hasColumns reports whether t has every one of cols.
func hasColumns(t *schemadiff.Table, cols []*schemadiff.Column) bool {
	for _, c := range cols {
		if t.Column(c.Name) == nil {
			return false
		}
	}
	return true
}

// clearTables empties every target table for --data replace before any row
// is inserted, children before parents: where foreign key checks stay on
// (PostgreSQL without superuser, CockroachDB), a parent can only be emptied
// once nothing references its rows. Tables whose structure change failed
// are left alone: the data phase skips them, so nothing would refill them.
// Tables it could not empty are added to failed under --continue-on-error.
func (p *Plan) clearTables(ctx context.Context, rc *Receipt, result func(string) *TableResult, failed map[string]bool) error {
	order := p.source.TablesInOrder()
	for i := len(order) - 1; i >= 0; i-- {
		st := order[i]
		if failed[st.Name] {
			continue
		}
		r := result(st.Name)
		err := func() error {
			if err := p.writer.Truncate(ctx, st.Name); err != nil {
				return sideError("target", err)
			}
			// A table the structure phase just created was empty.
			if r.Structure != "created" {
				r.Cleared = true
			}
			return nil
		}()
		if err != nil {
			if err := p.tableFailed(rc, r, failed, err); err != nil {
				return err
			}
		}
	}
	return nil
}
