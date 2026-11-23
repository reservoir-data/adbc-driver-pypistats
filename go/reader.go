package pypistats

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/adbc-drivers/driverbase-go/driverbase"
	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

type fetchResult struct {
	rec arrow.RecordBatch
	err error
}

// downloadsReader streams one record batch per package. Packages are fetched
// concurrently (bounded by maxConcurrency) but batches are emitted in the
// order the packages were configured.
type downloadsReader struct {
	refCount atomic.Int64
	schema   *arrow.Schema
	results  []chan fetchResult
	next     int
	cur      arrow.RecordBatch
	err      error
	cancel   context.CancelFunc
}

var _ array.RecordReader = (*downloadsReader)(nil)

func newDownloadsReader(
	ctx context.Context,
	alloc memory.Allocator,
	errs *driverbase.ErrorHelper,
	c *client,
	tbl table,
	packages []string,
	cols []int,
	maxConcurrency int,
) *downloadsReader {
	// The reader outlives the ExecuteQuery call, so detach from the caller's
	// cancellation; Release() cancels outstanding requests instead.
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))

	r := &downloadsReader{
		schema:  projectSchema(cols),
		results: make([]chan fetchResult, len(packages)),
		cancel:  cancel,
	}
	r.refCount.Store(1)

	sem := make(chan struct{}, max(1, maxConcurrency))
	for i, pkg := range packages {
		ch := make(chan fetchResult, 1)
		r.results[i] = ch
		go func() {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				ch <- fetchResult{err: errs.Cancelled("could not fetch %s/%s: %s", pkg, tbl.Endpoint, ctx.Err())}
				return
			}
			resp, err := c.fetch(ctx, pkg, tbl.Endpoint)
			if err != nil {
				ch <- fetchResult{err: wrapFetchError(errs, err, pkg, tbl)}
				return
			}
			rec, err := buildCategoryRecord(alloc, pkg, resp.Data)
			if err != nil {
				ch <- fetchResult{err: errs.WrapInvalidData(err, "could not parse %s/%s", pkg, tbl.Endpoint)}
				return
			}
			ch <- fetchResult{rec: rec}
		}()
	}
	return r
}

func wrapFetchError(errs *driverbase.ErrorHelper, err error, pkg string, tbl table) error {
	switch {
	case errors.Is(err, errPackageNotFound):
		return errs.WrapNotFound(err, "could not fetch package %q from table %s: not found on pypistats", pkg, tbl.Name)
	case errors.Is(err, context.Canceled):
		return errs.WrapCancelled(err, "could not fetch %s/%s", pkg, tbl.Endpoint)
	case errors.Is(err, context.DeadlineExceeded):
		return errs.WrapTimeout(err, "could not fetch %s/%s", pkg, tbl.Endpoint)
	default:
		return errs.WrapIO(err, "could not fetch %s/%s", pkg, tbl.Endpoint)
	}
}

// projectRecord narrows rec to cols and releases it. A nil cols keeps every column.
func projectRecord(rec arrow.RecordBatch, schema *arrow.Schema, cols []int) arrow.RecordBatch {
	if cols == nil {
		return rec
	}
	defer rec.Release()
	arrs := make([]arrow.Array, len(cols))
	for i, c := range cols {
		arrs[i] = rec.Column(c)
	}
	return array.NewRecordBatch(schema, arrs, rec.NumRows())
}

func buildCategoryRecord(alloc memory.Allocator, pkg string, rows []apiRow) (arrow.RecordBatch, error) {
	b := array.NewRecordBuilder(alloc, categorySchema)
	defer b.Release()

	pkgB := b.Field(0).(*array.StringBuilder)
	catB := b.Field(1).(*array.StringBuilder)
	dateB := b.Field(2).(*array.Date32Builder)
	dlB := b.Field(3).(*array.Uint32Builder)
	b.Reserve(len(rows))

	for _, row := range rows {
		d, err := time.Parse(time.DateOnly, row.Date)
		if err != nil {
			return nil, fmt.Errorf("could not parse date %q: %w", row.Date, err)
		}
		pkgB.Append(pkg)
		// category is part of the primary key, so it must not be null. pypistats
		// reports unknown categories as the string "null"; a JSON null is
		// normalized to the same value.
		if row.Category == nil {
			catB.Append("null")
		} else {
			catB.Append(*row.Category)
		}
		dateB.Append(arrow.Date32FromTime(d))
		dlB.Append(row.Downloads)
	}
	return b.NewRecordBatch(), nil
}

func (r *downloadsReader) Retain() { r.refCount.Add(1) }

func (r *downloadsReader) Release() {
	if r.refCount.Add(-1) != 0 {
		return
	}
	r.cancel()
	if r.cur != nil {
		r.cur.Release()
		r.cur = nil
	}
	// Free batches that were fetched but never consumed. Outstanding
	// requests were cancelled above, so this returns promptly.
	for _, ch := range r.results[r.next:] {
		if res := <-ch; res.rec != nil {
			res.rec.Release()
		}
	}
	r.next = len(r.results)
}

func (r *downloadsReader) Schema() *arrow.Schema { return r.schema }

func (r *downloadsReader) Next() bool {
	if r.cur != nil {
		r.cur.Release()
		r.cur = nil
	}
	for r.err == nil && r.next < len(r.results) {
		res := <-r.results[r.next]
		r.next++
		if res.err != nil {
			r.err = res.err
			r.cancel()
			return false
		}
		if res.rec.NumRows() == 0 {
			res.rec.Release()
			continue
		}
		r.cur = res.rec
		return true
	}
	return false
}

func (r *downloadsReader) RecordBatch() arrow.RecordBatch { return r.cur }

// Deprecated: use RecordBatch.
func (r *downloadsReader) Record() arrow.RecordBatch { return r.cur }

func (r *downloadsReader) Err() error { return r.err }
