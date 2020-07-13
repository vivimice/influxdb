package tsm1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/models"
	"github.com/influxdata/influxdb/v2/tsdb"
	"github.com/influxdata/influxdb/v2/tsdb/cursors"
	"github.com/influxdata/influxdb/v2/tsdb/seriesfile"
	"github.com/influxdata/influxql"
)

// cancelCheckInterval represents the period at which various schema calls
// will check for a canceled context. It is important this
// is not too frequent, or it could cause expensive context switches in
// tight loops.
const cancelCheckInterval = 5000

// TagValues returns an iterator which enumerates the values for the specific
// tagKey in the given bucket matching the predicate within the
// time range [start, end].
//
// TagValues will always return a StringIterator if there is no error.
//
// If the context is canceled before TagValues has finished processing, a non-nil
// error will be returned along with a partial result of the already scanned values.
func (e *Engine) TagValues(ctx context.Context, orgID, bucketID influxdb.ID, tagKey string, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if predicate == nil {
		return e.tagValuesNoPredicate(ctx, orgID, bucketID, nil, []byte(tagKey), start, end)
	}

	return e.tagValuesPredicate(ctx, orgID, bucketID, nil, []byte(tagKey), start, end, predicate)
}

func (e *Engine) tagValuesNoPredicate(ctx context.Context, orgID, bucketID influxdb.ID, measurement, tagKeyBytes []byte, start, end int64) (cursors.StringIterator, error) {
	tsmValues := make(map[string]struct{})
	var tags models.Tags

	orgBucket := tsdb.EncodeName(orgID, bucketID)

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	orgBucketEsc := models.EscapeMeasurement(orgBucket[:])

	tsmKeyPrefix := orgBucketEsc
	if len(measurement) > 0 {
		// append the measurement tag key to the prefix
		mt := models.Tags{models.NewTag(models.MeasurementTagKeyBytes, measurement)}
		tsmKeyPrefix = mt.AppendHashKey(tsmKeyPrefix)
		tsmKeyPrefix = append(tsmKeyPrefix, ',')
	}

	// TODO(sgc): extend prefix when filtering by \x00 == <measurement>

	var stats cursors.CursorStats
	var canceled bool

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		// Check the context before accessing each tsm file
		select {
		case <-ctx.Done():
			canceled = true
			return false
		default:
		}
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(tsmKeyPrefix, tsmKeyPrefix) {
			// TODO(sgc): create f.TimeRangeIterator(minKey, maxKey, start, end)
			iter := f.TimeRangeIterator(tsmKeyPrefix, start, end)
			for i := 0; iter.Next(); i++ {
				sfkey := iter.Key()
				if !bytes.HasPrefix(sfkey, tsmKeyPrefix) {
					// end of prefix
					break
				}

				key, _ := SeriesAndFieldFromCompositeKey(sfkey)
				tags = models.ParseTagsWithTags(key, tags[:0])
				curVal := tags.Get(tagKeyBytes)
				if len(curVal) == 0 {
					continue
				}

				if _, ok := tsmValues[string(curVal)]; ok {
					continue
				}

				if iter.HasData() {
					tsmValues[string(curVal)] = struct{}{}
				}
			}
			stats.Add(iter.Stats())
		}
		return true
	})

	if canceled {
		return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
	}

	var ts cursors.TimestampArray

	// With performance in mind, we explicitly do not check the context
	// while scanning the entries in the cache.
	tsmKeyprefixStr := string(tsmKeyPrefix)
	_ = e.Cache.ApplyEntryFn(func(sfkey string, entry *entry) error {
		if !strings.HasPrefix(sfkey, tsmKeyprefixStr) {
			return nil
		}

		// TODO(edd): consider the []byte() conversion here.
		key, _ := SeriesAndFieldFromCompositeKey([]byte(sfkey))
		tags = models.ParseTagsWithTags(key, tags[:0])
		curVal := tags.Get(tagKeyBytes)
		if len(curVal) == 0 {
			return nil
		}

		if _, ok := tsmValues[string(curVal)]; ok {
			return nil
		}

		ts.Timestamps = entry.AppendTimestamps(ts.Timestamps[:0])
		if ts.Len() > 0 {
			sort.Sort(&ts)

			stats.ScannedValues += ts.Len()
			stats.ScannedBytes += ts.Len() * 8 // sizeof timestamp

			if ts.Contains(start, end) {
				tsmValues[string(curVal)] = struct{}{}
			}
		}

		return nil
	})

	vals := make([]string, 0, len(tsmValues))
	for val := range tsmValues {
		vals = append(vals, val)
	}
	sort.Strings(vals)

	return cursors.NewStringSliceIteratorWithStats(vals, stats), nil
}

func (e *Engine) tagValuesPredicate(ctx context.Context, orgID, bucketID influxdb.ID, measurement, tagKeyBytes []byte, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if err := ValidateTagPredicate(predicate); err != nil {
		return nil, err
	}

	orgBucket := tsdb.EncodeName(orgID, bucketID)

	keys, err := e.findCandidateKeys(ctx, orgBucket[:], predicate)
	if err != nil {
		return cursors.EmptyStringIterator, err
	}

	if len(keys) == 0 {
		return cursors.EmptyStringIterator, nil
	}

	var files []TSMFile
	defer func() {
		for _, f := range files {
			f.Unref()
		}
	}()
	var iters []*TimeRangeIterator

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	orgBucketEsc := models.EscapeMeasurement(orgBucket[:])

	tsmKeyPrefix := orgBucketEsc
	if len(measurement) > 0 {
		// append the measurement tag key to the prefix
		mt := models.Tags{models.NewTag(models.MeasurementTagKeyBytes, measurement)}
		tsmKeyPrefix = mt.AppendHashKey(tsmKeyPrefix)
		tsmKeyPrefix = append(tsmKeyPrefix, ',')
	}

	var canceled bool

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		// Check the context before accessing each tsm file
		select {
		case <-ctx.Done():
			canceled = true
			return false
		default:
		}
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(tsmKeyPrefix, tsmKeyPrefix) {
			f.Ref()
			files = append(files, f)
			iters = append(iters, f.TimeRangeIterator(tsmKeyPrefix, start, end))
		}
		return true
	})

	var stats cursors.CursorStats

	if canceled {
		stats = statsFromIters(stats, iters)
		return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
	}

	tsmValues := make(map[string]struct{})

	// reusable buffers
	var (
		tags   models.Tags
		keybuf []byte
		sfkey  []byte
		ts     cursors.TimestampArray
	)

	for i := range keys {
		// to keep cache scans fast, check context every 'cancelCheckInterval' iteratons
		if i%cancelCheckInterval == 0 {
			select {
			case <-ctx.Done():
				stats = statsFromIters(stats, iters)
				return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
			default:
			}
		}

		_, tags = seriesfile.ParseSeriesKeyInto(keys[i], tags[:0])
		curVal := tags.Get(tagKeyBytes)
		if len(curVal) == 0 {
			continue
		}

		if _, ok := tsmValues[string(curVal)]; ok {
			continue
		}

		// orgBucketEsc is already escaped, so no need to use models.AppendMakeKey, which
		// unescapes and escapes the value again. The degenerate case is if the orgBucketEsc
		// has escaped values, causing two allocations per key
		keybuf = append(keybuf[:0], orgBucketEsc...)
		keybuf = tags.AppendHashKey(keybuf)
		sfkey = AppendSeriesFieldKeyBytes(sfkey[:0], keybuf, tags.Get(models.FieldKeyTagKeyBytes))

		ts.Timestamps = e.Cache.AppendTimestamps(sfkey, ts.Timestamps[:0])
		if ts.Len() > 0 {
			sort.Sort(&ts)

			stats.ScannedValues += ts.Len()
			stats.ScannedBytes += ts.Len() * 8 // sizeof timestamp

			if ts.Contains(start, end) {
				tsmValues[string(curVal)] = struct{}{}
			}
			continue
		}

		for _, iter := range iters {
			if exact, _ := iter.Seek(sfkey); !exact {
				continue
			}

			if iter.HasData() {
				tsmValues[string(curVal)] = struct{}{}
				break
			}
		}
	}

	vals := make([]string, 0, len(tsmValues))
	for val := range tsmValues {
		vals = append(vals, val)
	}
	sort.Strings(vals)

	stats = statsFromIters(stats, iters)
	return cursors.NewStringSliceIteratorWithStats(vals, stats), err
}

func (e *Engine) findCandidateKeys(ctx context.Context, orgBucket []byte, predicate influxql.Expr) ([][]byte, error) {
	// determine candidate series keys
	sitr, err := e.index.MeasurementSeriesByExprIterator(orgBucket, predicate)
	if err != nil {
		return nil, err
	} else if sitr == nil {
		return nil, nil
	}
	defer sitr.Close()

	var keys [][]byte
	for i := 0; ; i++ {
		// to keep series file index scans fast,
		// check context every 'cancelCheckInterval' iteratons
		if i%cancelCheckInterval == 0 {
			select {
			case <-ctx.Done():
				return keys, ctx.Err()
			default:
			}
		}

		elem, err := sitr.Next()
		if err != nil {
			return nil, err
		} else if elem.SeriesID.IsZero() {
			break
		}

		key := e.sfile.SeriesKey(elem.SeriesID)
		if len(key) == 0 {
			continue
		}
		keys = append(keys, key)
	}

	return keys, nil
}

// TagKeys returns an iterator which enumerates the tag keys for the given
// bucket matching the predicate within the time range [start, end].
//
// TagKeys will always return a StringIterator if there is no error.
//
// If the context is canceled before TagKeys has finished processing, a non-nil
// error will be returned along with a partial result of the already scanned keys.
func (e *Engine) TagKeys(ctx context.Context, orgID, bucketID influxdb.ID, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if predicate == nil {
		return e.tagKeysNoPredicate(ctx, orgID, bucketID, nil, start, end)
	}

	return e.tagKeysPredicate(ctx, orgID, bucketID, nil, start, end, predicate)
}

func (e *Engine) tagKeysNoPredicate(ctx context.Context, orgID, bucketID influxdb.ID, measurement []byte, start, end int64) (cursors.StringIterator, error) {
	var tags models.Tags

	orgBucket := tsdb.EncodeName(orgID, bucketID)

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	orgBucketEsc := models.EscapeMeasurement(orgBucket[:])

	tsmKeyPrefix := orgBucketEsc
	if len(measurement) > 0 {
		// append the measurement tag key to the prefix
		mt := models.Tags{models.NewTag(models.MeasurementTagKeyBytes, measurement)}
		tsmKeyPrefix = mt.AppendHashKey(tsmKeyPrefix)
		tsmKeyPrefix = append(tsmKeyPrefix, ',')
	}

	var keyset models.TagKeysSet

	// TODO(sgc): extend prefix when filtering by \x00 == <measurement>

	var stats cursors.CursorStats
	var canceled bool

	var files unrefs
	defer func() { files.Unref() }()

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		// Check the context before touching each tsm file
		select {
		case <-ctx.Done():
			canceled = true
			return false
		default:
		}

		var hasRef bool
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(tsmKeyPrefix, tsmKeyPrefix) {
			// TODO(sgc): create f.TimeRangeIterator(minKey, maxKey, start, end)
			iter := f.TimeRangeIterator(tsmKeyPrefix, start, end)
			for i := 0; iter.Next(); i++ {
				sfkey := iter.Key()
				if !bytes.HasPrefix(sfkey, tsmKeyPrefix) {
					// end of prefix
					break
				}

				key, _ := SeriesAndFieldFromCompositeKey(sfkey)
				tags = models.ParseTagsWithTags(key, tags[:0])
				if keyset.IsSupersetKeys(tags) {
					continue
				}

				if iter.HasData() {
					keyset.UnionKeys(tags)

					// Add reference to ensure tags are valid for the outer function.
					if !hasRef {
						f.Ref()
						files, hasRef = append(files, f), true
					}
				}
			}
			stats.Add(iter.Stats())
		}
		return true
	})

	if canceled {
		return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
	}

	var ts cursors.TimestampArray

	// With performance in mind, we explicitly do not check the context
	// while scanning the entries in the cache.
	tsmKeyprefixStr := string(tsmKeyPrefix)
	_ = e.Cache.ApplyEntryFn(func(sfkey string, entry *entry) error {
		if !strings.HasPrefix(sfkey, tsmKeyprefixStr) {
			return nil
		}

		// TODO(edd): consider []byte conversion here.
		key, _ := SeriesAndFieldFromCompositeKey([]byte(sfkey))
		tags = models.ParseTagsWithTags(key, tags[:0])
		if keyset.IsSupersetKeys(tags) {
			return nil
		}

		ts.Timestamps = entry.AppendTimestamps(ts.Timestamps[:0])
		if ts.Len() > 0 {
			sort.Sort(&ts)

			stats.ScannedValues += ts.Len()
			stats.ScannedBytes += ts.Len() * 8 // sizeof timestamp

			if ts.Contains(start, end) {
				keyset.UnionKeys(tags)
			}
		}

		return nil
	})

	return cursors.NewStringSliceIteratorWithStats(keyset.Keys(), stats), nil
}

func (e *Engine) tagKeysPredicate(ctx context.Context, orgID, bucketID influxdb.ID, measurement []byte, start, end int64, predicate influxql.Expr) (cursors.StringIterator, error) {
	if err := ValidateTagPredicate(predicate); err != nil {
		return nil, err
	}

	orgBucket := tsdb.EncodeName(orgID, bucketID)

	keys, err := e.findCandidateKeys(ctx, orgBucket[:], predicate)
	if err != nil {
		return cursors.EmptyStringIterator, err
	}

	if len(keys) == 0 {
		return cursors.EmptyStringIterator, nil
	}

	var files []TSMFile
	defer func() {
		for _, f := range files {
			f.Unref()
		}
	}()
	var iters []*TimeRangeIterator

	// TODO(edd): we need to clean up how we're encoding the prefix so that we
	// don't have to remember to get it right everywhere we need to touch TSM data.
	orgBucketEsc := models.EscapeMeasurement(orgBucket[:])

	tsmKeyPrefix := orgBucketEsc
	if len(measurement) > 0 {
		// append the measurement tag key to the prefix
		mt := models.Tags{models.NewTag(models.MeasurementTagKeyBytes, measurement)}
		tsmKeyPrefix = mt.AppendHashKey(tsmKeyPrefix)
		tsmKeyPrefix = append(tsmKeyPrefix, ',')
	}

	var canceled bool

	e.FileStore.ForEachFile(func(f TSMFile) bool {
		// Check the context before touching each tsm file
		select {
		case <-ctx.Done():
			canceled = true
			return false
		default:
		}
		if f.OverlapsTimeRange(start, end) && f.OverlapsKeyPrefixRange(tsmKeyPrefix, tsmKeyPrefix) {
			f.Ref()
			files = append(files, f)
			iters = append(iters, f.TimeRangeIterator(tsmKeyPrefix, start, end))
		}
		return true
	})

	var stats cursors.CursorStats

	if canceled {
		stats = statsFromIters(stats, iters)
		return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
	}

	var keyset models.TagKeysSet

	// reusable buffers
	var (
		tags   models.Tags
		keybuf []byte
		sfkey  []byte
		ts     cursors.TimestampArray
	)

	for i := range keys {
		// to keep cache scans fast, check context every 'cancelCheckInterval' iteratons
		if i%cancelCheckInterval == 0 {
			select {
			case <-ctx.Done():
				stats = statsFromIters(stats, iters)
				return cursors.NewStringSliceIteratorWithStats(nil, stats), ctx.Err()
			default:
			}
		}

		_, tags = seriesfile.ParseSeriesKeyInto(keys[i], tags[:0])
		if keyset.IsSupersetKeys(tags) {
			continue
		}

		// orgBucketEsc is already escaped, so no need to use models.AppendMakeKey, which
		// unescapes and escapes the value again. The degenerate case is if the orgBucketEsc
		// has escaped values, causing two allocations per key
		keybuf = append(keybuf[:0], orgBucketEsc...)
		keybuf = tags.AppendHashKey(keybuf)
		sfkey = AppendSeriesFieldKeyBytes(sfkey[:0], keybuf, tags.Get(models.FieldKeyTagKeyBytes))

		ts.Timestamps = e.Cache.AppendTimestamps(sfkey, ts.Timestamps[:0])
		if ts.Len() > 0 {
			sort.Sort(&ts)

			stats.ScannedValues += ts.Len()
			stats.ScannedBytes += ts.Len() * 8 // sizeof timestamp

			if ts.Contains(start, end) {
				keyset.UnionKeys(tags)
				continue
			}
		}

		for _, iter := range iters {
			if exact, _ := iter.Seek(sfkey); !exact {
				continue
			}

			if iter.HasData() {
				keyset.UnionKeys(tags)
				break
			}
		}
	}

	stats = statsFromIters(stats, iters)
	return cursors.NewStringSliceIteratorWithStats(keyset.Keys(), stats), err
}

func statsFromIters(stats cursors.CursorStats, iters []*TimeRangeIterator) cursors.CursorStats {
	for _, iter := range iters {
		stats.Add(iter.Stats())
	}
	return stats
}

var (
	errUnexpectedTagComparisonOperator = errors.New("unexpected tag comparison operator")
	errNotImplemented                  = errors.New("not implemented")
)

func ValidateTagPredicate(expr influxql.Expr) (err error) {
	influxql.WalkFunc(expr, func(node influxql.Node) {
		if err != nil {
			return
		}

		switch n := node.(type) {
		case *influxql.BinaryExpr:
			switch n.Op {
			case influxql.EQ, influxql.EQREGEX, influxql.NEQREGEX, influxql.NEQ, influxql.OR, influxql.AND:
			default:
				err = errUnexpectedTagComparisonOperator
			}

			switch r := n.LHS.(type) {
			case *influxql.VarRef:
			case *influxql.BinaryExpr:
			case *influxql.ParenExpr:
			default:
				err = fmt.Errorf("binary expression: LHS must be tag key reference, got: %T", r)
			}

			switch r := n.RHS.(type) {
			case *influxql.StringLiteral:
			case *influxql.RegexLiteral:
			case *influxql.BinaryExpr:
			case *influxql.ParenExpr:
			default:
				err = fmt.Errorf("binary expression: RHS must be string or regex, got: %T", r)
			}
		}
	})
	return err
}

func ValidateMeasurementNamesTagPredicate(expr influxql.Expr) (err error) {
	influxql.WalkFunc(expr, func(node influxql.Node) {
		if err != nil {
			return
		}

		switch n := node.(type) {
		case *influxql.BinaryExpr:
			switch n.Op {
			case influxql.EQ, influxql.EQREGEX, influxql.OR, influxql.AND:
			case influxql.NEQREGEX, influxql.NEQ:
				err = errNotImplemented
			default:
				err = errUnexpectedTagComparisonOperator
			}

			switch r := n.LHS.(type) {
			case *influxql.VarRef:
			case *influxql.BinaryExpr:
			case *influxql.ParenExpr:
			default:
				err = fmt.Errorf("binary expression: LHS must be tag key reference, got: %T", r)
			}

			switch r := n.RHS.(type) {
			case *influxql.StringLiteral:
			case *influxql.RegexLiteral:
			case *influxql.BinaryExpr:
			case *influxql.ParenExpr:
			default:
				err = fmt.Errorf("binary expression: RHS must be string or regex, got: %T", r)
			}
		}
	})
	return err
}
