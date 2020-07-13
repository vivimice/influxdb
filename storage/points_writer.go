package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/influxdata/influxdb/v2"
	"github.com/influxdata/influxdb/v2/models"
	"github.com/influxdata/influxdb/v2/tsdb"
)

// PointsWriter describes the ability to write points into a storage engine.
type PointsWriter interface {
	WritePoints(context.Context, []models.Point) error
}

// LoggingPointsWriter wraps an underlying points writer but writes logs to
// another bucket when an error occurs.
type LoggingPointsWriter struct {
	// Wrapped points writer. Errored writes from here will be logged.
	Underlying PointsWriter

	// Service used to look up logging bucket.
	BucketFinder BucketFinder

	// Name of the bucket to log to.
	LogBucketName string
}

// WritePoints writes points to the underlying PointsWriter. Logs on error.
func (w *LoggingPointsWriter) WritePoints(ctx context.Context, p []models.Point) error {
	if len(p) == 0 {
		return nil
	}

	// Write to underlying writer and exit immediately if successful.
	err := w.Underlying.WritePoints(ctx, p)
	if err == nil {
		return nil
	}

	// Find organizationID from points
	orgID, _ := tsdb.DecodeNameSlice(p[0].Name())

	// Attempt to lookup log bucket.
	bkts, n, e := w.BucketFinder.FindBuckets(ctx, influxdb.BucketFilter{
		OrganizationID: &orgID,
		Name:           &w.LogBucketName,
	})
	if e != nil {
		return e
	} else if n == 0 {
		return fmt.Errorf("logging bucket not found: %q", w.LogBucketName)
	}

	// Log error to bucket.
	name := tsdb.EncodeName(orgID, bkts[0].ID)
	pt, e := models.NewPoint(
		string(name[:]),
		models.NewTags(map[string]string{
			models.MeasurementTagKey: "write_errors",
			models.FieldKeyTagKey:    "error"},
		),
		models.Fields{"error": err.Error()},
		time.Now(),
	)
	if e != nil {
		return e
	}
	if e := w.Underlying.WritePoints(ctx, []models.Point{pt}); e != nil {
		return e
	}

	return err
}

type BufferedPointsWriter struct {
	buf []models.Point
	n   int
	wr  PointsWriter
	err error
}

func NewBufferedPointsWriter(size int, pointswriter PointsWriter) *BufferedPointsWriter {
	return &BufferedPointsWriter{
		buf: make([]models.Point, size),
		wr:  pointswriter,
	}
}

// WritePoints writes the points to the underlying PointsWriter.
func (b *BufferedPointsWriter) WritePoints(ctx context.Context, p []models.Point) error {
	for len(p) > b.Available() && b.err == nil {
		if b.Buffered() == 0 {
			// Large write, empty buffer.
			// Write directly from p to avoid copy.
			b.err = b.wr.WritePoints(ctx, p)
			return b.err
		}
		n := copy(b.buf[b.n:], p)
		b.n += n
		b.err = b.Flush(ctx)
		p = p[n:]
	}
	if b.err != nil {
		return b.err
	}
	b.n += copy(b.buf[b.n:], p)
	return nil
}

// Available returns how many models.Points are unused in the buffer.
func (b *BufferedPointsWriter) Available() int { return len(b.buf) - b.n }

// Buffered returns the number of models.Points that have been written into the current buffer.
func (b *BufferedPointsWriter) Buffered() int { return b.n }

// Flush writes any buffered data to the underlying PointsWriter.
func (b *BufferedPointsWriter) Flush(ctx context.Context) error {
	if b.err != nil {
		return b.err
	}
	if b.n == 0 {
		return nil
	}

	b.err = b.wr.WritePoints(ctx, b.buf[:b.n])
	if b.err != nil {
		return b.err
	}
	b.n = 0
	return nil
}
