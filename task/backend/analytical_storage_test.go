package backend_test

import (
	"context"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/influxdata/flux"
	"github.com/influxdata/influxdb/v2"
	icontext "github.com/influxdata/influxdb/v2/context"
	"github.com/influxdata/influxdb/v2/inmem"
	"github.com/influxdata/influxdb/v2/kv"
	"github.com/influxdata/influxdb/v2/kv/migration/all"
	"github.com/influxdata/influxdb/v2/mock"
	"github.com/influxdata/influxdb/v2/query"
	_ "github.com/influxdata/influxdb/v2/query/builtin"
	"github.com/influxdata/influxdb/v2/query/control"
	"github.com/influxdata/influxdb/v2/query/fluxlang"
	stdlib "github.com/influxdata/influxdb/v2/query/stdlib/influxdata/influxdb"
	"github.com/influxdata/influxdb/v2/storage"
	storageflux "github.com/influxdata/influxdb/v2/storage/flux"
	"github.com/influxdata/influxdb/v2/storage/readservice"
	"github.com/influxdata/influxdb/v2/task/backend"
	"github.com/influxdata/influxdb/v2/task/servicetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

func TestAnalyticalStore(t *testing.T) {
	servicetest.TestTaskService(
		t,
		func(t *testing.T) (*servicetest.System, context.CancelFunc) {
			ctx, cancelFunc := context.WithCancel(context.Background())
			logger := zaptest.NewLogger(t)
			store := inmem.NewKVStore()
			if err := all.Up(ctx, logger, store); err != nil {
				t.Fatal(err)
			}

			svc := kv.NewService(logger, store, kv.ServiceConfig{
				FluxLanguageService: fluxlang.DefaultService,
			})

			var (
				ab       = newAnalyticalBackend(t, svc, svc)
				rr       = backend.NewStoragePointsWriterRecorder(logger, ab.PointsWriter())
				svcStack = backend.NewAnalyticalRunStorage(logger, svc, svc, svc, rr, ab.QueryService())
			)

			go func() {
				<-ctx.Done()
				ab.Close(t)
			}()

			authCtx := icontext.SetAuthorizer(ctx, &influxdb.Authorization{
				Permissions: influxdb.OperPermissions(),
			})

			return &servicetest.System{
				TaskControlService: svcStack,
				TaskService:        svcStack,
				I:                  svc,
				Ctx:                authCtx,
			}, cancelFunc
		},
	)
}

func TestDeduplicateRuns(t *testing.T) {
	logger := zaptest.NewLogger(t)
	store := inmem.NewKVStore()
	if err := all.Up(context.Background(), logger, store); err != nil {
		t.Fatal(err)
	}

	svc := kv.NewService(logger, store)

	ab := newAnalyticalBackend(t, svc, svc)
	defer ab.Close(t)

	mockTS := &mock.TaskService{
		FindTaskByIDFn: func(context.Context, influxdb.ID) (*influxdb.Task, error) {
			return &influxdb.Task{ID: 1, OrganizationID: 20}, nil
		},
		FindRunsFn: func(context.Context, influxdb.RunFilter) ([]*influxdb.Run, int, error) {
			return []*influxdb.Run{
				&influxdb.Run{ID: 2, Status: "started"},
			}, 1, nil
		},
	}
	mockTCS := &mock.TaskControlService{
		FinishRunFn: func(ctx context.Context, taskID, runID influxdb.ID) (*influxdb.Run, error) {
			return &influxdb.Run{ID: 2, TaskID: 1, Status: "success", ScheduledFor: time.Now(), StartedAt: time.Now().Add(1), FinishedAt: time.Now().Add(2)}, nil
		},
	}
	mockBS := mock.NewBucketService()

	svcStack := backend.NewAnalyticalStorage(zaptest.NewLogger(t), mockTS, mockBS, mockTCS, ab.PointsWriter(), ab.QueryService())

	_, err := svcStack.FinishRun(context.Background(), 1, 2)
	if err != nil {
		t.Fatal(err)
	}

	runs, _, err := svcStack.FindRuns(context.Background(), influxdb.RunFilter{Task: 1})
	if err != nil {
		t.Fatal(err)
	}

	if len(runs) != 1 {
		t.Fatalf("expected 1 run but got %d", len(runs))
	}

	if runs[0].Status != "success" {
		t.Fatalf("expected the deduped run to be 'success', got: %s", runs[0].Status)
	}
}

type analyticalBackend struct {
	queryController *control.Controller
	rootDir         string
	storageEngine   *storage.Engine
}

func (ab *analyticalBackend) PointsWriter() storage.PointsWriter {
	return ab.storageEngine
}

func (ab *analyticalBackend) QueryService() query.QueryService {
	return query.QueryServiceBridge{AsyncQueryService: ab.queryController}
}

func (ab *analyticalBackend) Close(t *testing.T) {
	if err := ab.queryController.Shutdown(context.Background()); err != nil {
		t.Error(err)
	}
	if err := ab.storageEngine.Close(); err != nil {
		t.Error(err)
	}
	if err := os.RemoveAll(ab.rootDir); err != nil {
		t.Error(err)
	}
}

func newAnalyticalBackend(t *testing.T, orgSvc influxdb.OrganizationService, bucketSvc influxdb.BucketService) *analyticalBackend {
	// Mostly copied out of cmd/influxd/main.go.
	logger := zaptest.NewLogger(t)

	rootDir, err := ioutil.TempDir("", "task-logreaderwriter-")
	if err != nil {
		t.Fatal(err)
	}

	engine := storage.NewEngine(rootDir, storage.NewConfig())
	engine.WithLogger(logger)

	if err := engine.Open(context.Background()); err != nil {
		t.Fatal(err)
	}

	defer func() {
		if t.Failed() {
			engine.Close()
			os.RemoveAll(rootDir)
		}
	}()

	const (
		concurrencyQuota         = 10
		memoryBytesQuotaPerQuery = 1e6
		queueSize                = 10
	)

	// TODO(adam): do we need a proper secret service here?
	reader := storageflux.NewReader(readservice.NewStore(engine))
	deps, err := stdlib.NewDependencies(reader, engine, bucketSvc, orgSvc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	cc := control.Config{
		ExecutorDependencies:     []flux.Dependency{deps},
		ConcurrencyQuota:         concurrencyQuota,
		MemoryBytesQuotaPerQuery: int64(memoryBytesQuotaPerQuery),
		QueueSize:                queueSize,
		Logger:                   logger.With(zap.String("service", "storage-reads")),
	}

	queryController, err := control.New(cc)
	if err != nil {
		t.Fatal(err)
	}

	return &analyticalBackend{
		queryController: queryController,
		rootDir:         rootDir,
		storageEngine:   engine,
	}
}
