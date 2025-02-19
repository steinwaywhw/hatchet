package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/go-co-op/gocron/v2"
	"github.com/goccy/go-json"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/hatchet-dev/hatchet/internal/datautils"
	"github.com/hatchet-dev/hatchet/internal/datautils/merge"
	"github.com/hatchet-dev/hatchet/internal/msgqueue"
	"github.com/hatchet-dev/hatchet/internal/services/controllers/partition"
	"github.com/hatchet-dev/hatchet/internal/services/shared/recoveryutils"
	"github.com/hatchet-dev/hatchet/internal/services/shared/tasktypes"
	"github.com/hatchet-dev/hatchet/internal/telemetry"
	"github.com/hatchet-dev/hatchet/internal/telemetry/servertel"
	"github.com/hatchet-dev/hatchet/pkg/config/shared"
	hatcheterrors "github.com/hatchet-dev/hatchet/pkg/errors"
	"github.com/hatchet-dev/hatchet/pkg/logger"
	"github.com/hatchet-dev/hatchet/pkg/repository"
	"github.com/hatchet-dev/hatchet/pkg/repository/prisma/dbsqlc"
	"github.com/hatchet-dev/hatchet/pkg/repository/prisma/sqlchelpers"
)

type JobsController interface {
	Start(ctx context.Context) error
}

type JobsControllerImpl struct {
	mq             msgqueue.MessageQueue
	l              *zerolog.Logger
	queueLogger    *zerolog.Logger
	pgxStatsLogger *zerolog.Logger
	repo           repository.EngineRepository
	dv             datautils.DataDecoderValidator
	s              gocron.Scheduler
	a              *hatcheterrors.Wrapped
	p              *partition.Partition

	reassignMutexes sync.Map
	timeoutMutexes  sync.Map
}

type JobsControllerOpt func(*JobsControllerOpts)

type JobsControllerOpts struct {
	mq             msgqueue.MessageQueue
	l              *zerolog.Logger
	repo           repository.EngineRepository
	dv             datautils.DataDecoderValidator
	alerter        hatcheterrors.Alerter
	p              *partition.Partition
	queueLogger    *zerolog.Logger
	pgxStatsLogger *zerolog.Logger
}

func defaultJobsControllerOpts() *JobsControllerOpts {
	l := logger.NewDefaultLogger("jobs-controller")
	alerter := hatcheterrors.NoOpAlerter{}

	queueLogger := logger.NewDefaultLogger("queue")
	pgxStatsLogger := logger.NewDefaultLogger("pgx-stats")

	return &JobsControllerOpts{
		l:              &l,
		dv:             datautils.NewDataDecoderValidator(),
		alerter:        alerter,
		queueLogger:    &queueLogger,
		pgxStatsLogger: &pgxStatsLogger,
	}
}

func WithMessageQueue(mq msgqueue.MessageQueue) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.mq = mq
	}
}

func WithLogger(l *zerolog.Logger) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.l = l
	}
}

func WithQueueLoggerConfig(lc *shared.LoggerConfigFile) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		l := logger.NewStdErr(lc, "queue")
		opts.queueLogger = &l
	}
}

func WithPgxStatsLoggerConfig(lc *shared.LoggerConfigFile) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		l := logger.NewStdErr(lc, "pgx-stats")
		opts.pgxStatsLogger = &l
	}
}

func WithAlerter(a hatcheterrors.Alerter) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.alerter = a
	}
}

func WithRepository(r repository.EngineRepository) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.repo = r
	}
}

func WithPartition(p *partition.Partition) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.p = p
	}
}

func WithDataDecoderValidator(dv datautils.DataDecoderValidator) JobsControllerOpt {
	return func(opts *JobsControllerOpts) {
		opts.dv = dv
	}
}

func New(fs ...JobsControllerOpt) (*JobsControllerImpl, error) {
	opts := defaultJobsControllerOpts()

	for _, f := range fs {
		f(opts)
	}

	if opts.mq == nil {
		return nil, fmt.Errorf("task queue is required. use WithMessageQueue")
	}

	if opts.repo == nil {
		return nil, fmt.Errorf("repository is required. use WithRepository")
	}

	if opts.p == nil {
		return nil, errors.New("partition is required. use WithPartition")
	}

	newLogger := opts.l.With().Str("service", "jobs-controller").Logger()
	opts.l = &newLogger

	s, err := gocron.NewScheduler(gocron.WithLocation(time.UTC))

	if err != nil {
		return nil, fmt.Errorf("could not create scheduler: %w", err)
	}

	a := hatcheterrors.NewWrapped(opts.alerter)
	a.WithData(map[string]interface{}{"service": "jobs-controller"})

	return &JobsControllerImpl{
		mq:             opts.mq,
		l:              opts.l,
		queueLogger:    opts.queueLogger,
		pgxStatsLogger: opts.pgxStatsLogger,
		repo:           opts.repo,
		dv:             opts.dv,
		s:              s,
		a:              a,
		p:              opts.p,
	}, nil
}

func (jc *JobsControllerImpl) Start() (func() error, error) {
	ctx, cancel := context.WithCancel(context.Background())

	wg := sync.WaitGroup{}

	startedAt := time.Now()

	_, err := jc.s.NewJob(
		gocron.DurationJob(time.Second*15),
		gocron.NewTask(
			jc.runPgStat(),
		),
	)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not schedule step run requeue: %w", err)
	}

	_, err = jc.s.NewJob(
		gocron.DurationJob(time.Second*1),
		gocron.NewTask(
			jc.runStepRunReassign(ctx, startedAt),
		),
	)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not schedule step run reassign: %w", err)
	}

	_, err = jc.s.NewJob(
		gocron.DurationJob(time.Second*1),
		gocron.NewTask(
			jc.runStepRunTimeout(ctx),
		),
	)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not schedule step run timeout: %w", err)
	}

	jc.s.Start()

	f := func(task *msgqueue.Message) error {
		wg.Add(1)
		defer wg.Done()

		err := jc.handleTask(context.Background(), task)
		if err != nil {
			jc.l.Error().Err(err).Msg("could not handle job task")
			return jc.a.WrapErr(fmt.Errorf("could not handle job task: %w", err), map[string]interface{}{"task_id": task.ID}) // nolint: errcheck
		}

		return nil
	}

	cleanupQueue, err := jc.mq.Subscribe(msgqueue.JOB_PROCESSING_QUEUE, f, msgqueue.NoOpHook)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not subscribe to job processing queue: %w", err)
	}

	q, err := newQueue(jc.mq, jc.l, jc.queueLogger, jc.repo, jc.dv, jc.a, jc.p)

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not create partition: %w", err)
	}

	partitionCleanup, err := q.Start()

	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not start partition: %w", err)
	}

	cleanup := func() error {
		cancel()

		if err := cleanupQueue(); err != nil {
			return fmt.Errorf("could not cleanup job processing queue: %w", err)
		}

		if err := jc.s.Shutdown(); err != nil {
			return fmt.Errorf("could not shutdown scheduler: %w", err)
		}

		if err := partitionCleanup(); err != nil {
			return fmt.Errorf("could not cleanup partition: %w", err)
		}

		wg.Wait()

		return nil
	}

	return cleanup, nil
}

func (ec *JobsControllerImpl) handleTask(ctx context.Context, task *msgqueue.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			recoverErr := recoveryutils.RecoverWithAlert(ec.l, ec.a, r)

			if recoverErr != nil {
				err = recoverErr
			}
		}
	}()

	switch task.ID {
	case "job-run-queued":
		return ec.handleJobRunQueued(ctx, task)
	case "job-run-cancelled":
		return ec.handleJobRunCancelled(ctx, task)
	case "step-run-replay":
		return ec.handleStepRunReplay(ctx, task)
	case "step-run-retry":
		return ec.handleStepRunRetry(ctx, task)
	case "step-run-queued":
		return ec.handleStepRunQueued(ctx, task)
	case "step-run-started":
		return ec.handleStepRunStarted(ctx, task)
	case "step-run-finished":
		return ec.handleStepRunFinished(ctx, task)
	case "step-run-failed":
		return ec.handleStepRunFailed(ctx, task)
	case "step-run-cancel":
		return ec.handleStepRunCancel(ctx, task)
	case "step-run-timed-out":
		return ec.handleStepRunTimedOut(ctx, task)
	}
	return fmt.Errorf("unknown task: %s", task.ID)
}

func (ec *JobsControllerImpl) handleJobRunQueued(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-job-run-queued", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.JobRunQueuedTaskPayload{}
	metadata := tasktypes.JobRunQueuedTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	err = ec.repo.JobRun().SetJobRunStatusRunning(ctx, metadata.TenantId, payload.JobRunId)

	if err != nil {
		return fmt.Errorf("could not set job run status to running: %w", err)
	}

	// list the step runs which are startable
	startableStepRuns, err := ec.repo.StepRun().ListStartableStepRuns(ctx, metadata.TenantId, payload.JobRunId, nil)

	if err != nil {
		return fmt.Errorf("could not list startable step runs: %w", err)
	}

	g := new(errgroup.Group)

	for _, stepRun := range startableStepRuns {
		stepRunCp := stepRun

		g.Go(func() error {
			return ec.mq.AddMessage(
				ctx,
				msgqueue.JOB_PROCESSING_QUEUE,
				tasktypes.StepRunQueuedToTask(stepRunCp),
			)
		})
	}

	err = g.Wait()

	if err != nil {
		ec.l.Err(err).Msg("could not run job run queued")
		return err
	}

	return nil
}

func (ec *JobsControllerImpl) handleJobRunCancelled(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-job-run-cancelled", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.JobRunCancelledTaskPayload{}
	metadata := tasktypes.JobRunCancelledTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	stepRuns, err := ec.repo.StepRun().ListStepRuns(ctx, metadata.TenantId, &repository.ListStepRunsOpts{
		JobRunId: &payload.JobRunId,
	})

	if err != nil {
		return fmt.Errorf("could not list step runs: %w", err)
	}

	g := new(errgroup.Group)

	for _, stepRun := range stepRuns {
		stepRunCp := stepRun

		reason := "JOB_RUN_CANCELLED"

		if payload.Reason != nil {
			reason = *payload.Reason
		}

		g.Go(func() error {
			return ec.mq.AddMessage(
				ctx,
				msgqueue.JOB_PROCESSING_QUEUE,
				tasktypes.StepRunCancelToTask(stepRunCp, reason),
			)
		})
	}

	err = g.Wait()

	if err != nil {
		ec.l.Err(err).Msg("could not run job run cancelled")
		return err
	}

	return nil
}

func (ec *JobsControllerImpl) handleStepRunRetry(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-retry", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunRetryTaskPayload{}
	metadata := tasktypes.StepRunRetryTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	err = ec.repo.StepRun().ArchiveStepRunResult(ctx, metadata.TenantId, payload.StepRunId, payload.Error)

	if err != nil {
		return fmt.Errorf("could not archive step run result: %w", err)
	}

	stepRun, err := ec.repo.StepRun().GetStepRunForEngine(ctx, metadata.TenantId, payload.StepRunId)

	if err != nil {
		return fmt.Errorf("could not get step run: %w", err)
	}

	retryCount := int(stepRun.SRRetryCount) + 1

	// write an event
	defer ec.repo.StepRun().DeferredStepRunEvent(metadata.TenantId, sqlchelpers.UUIDToStr(stepRun.SRID), repository.CreateStepRunEventOpts{
		EventReason: repository.StepRunEventReasonPtr(dbsqlc.StepRunEventReasonRETRYING),
		EventMessage: repository.StringPtr(
			fmt.Sprintf("Retrying step run. This is retry %d / %d", retryCount, stepRun.StepRetries),
		),
	})

	return ec.queueStepRun(ctx, metadata.TenantId, sqlchelpers.UUIDToStr(stepRun.StepId), sqlchelpers.UUIDToStr(stepRun.SRID), true)
}

// handleStepRunReplay replays a step run from scratch - it resets the workflow run state, job run state, and
// all cancelled step runs which are children of the step run being replayed.
func (ec *JobsControllerImpl) handleStepRunReplay(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-replay", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunReplayTaskPayload{}
	metadata := tasktypes.StepRunReplayTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	err = ec.repo.StepRun().ArchiveStepRunResult(ctx, metadata.TenantId, payload.StepRunId, nil)

	if err != nil {
		return fmt.Errorf("could not archive step run result: %w", err)
	}

	stepRun, err := ec.repo.StepRun().GetStepRunForEngine(ctx, metadata.TenantId, payload.StepRunId)

	if err != nil {
		return fmt.Errorf("could not get step run: %w", err)
	}

	data, err := ec.repo.StepRun().GetStepRunDataForEngine(ctx, metadata.TenantId, payload.StepRunId)

	if err != nil {
		return fmt.Errorf("could not get step run data: %w", err)
	}

	var inputBytes []byte

	// update the input schema for the step run based on the new input
	if payload.InputData != "" {
		inputBytes = []byte(payload.InputData)

		// Merge the existing input data with the new input data. We don't blindly trust the
		// input data because the user could have deleted fields that are required by the step.
		// A better solution would be to validate the user input ahead of time.
		// NOTE: this is an expensive operation.
		if currentInput := data.Input; len(currentInput) > 0 {
			inputMap, err := datautils.JSONBytesToMap([]byte(payload.InputData))

			if err != nil {
				return fmt.Errorf("could not convert input data to map: %w", err)
			}

			currentInputMap, err := datautils.JSONBytesToMap(currentInput)

			if err != nil {
				return fmt.Errorf("could not convert current input to map: %w", err)
			}

			currentInputOverridesMap, ok1 := currentInputMap["overrides"].(map[string]interface{})
			inputOverridesMap, ok2 := inputMap["overrides"].(map[string]interface{})

			if ok1 && ok2 {
				mergedInputOverrides := merge.MergeMaps(currentInputOverridesMap, inputOverridesMap)

				inputMap["overrides"] = mergedInputOverrides

				mergedInputBytes, err := json.Marshal(inputMap)

				if err != nil {
					return fmt.Errorf("could not marshal merged input: %w", err)
				}

				inputBytes = mergedInputBytes
			}
		}
	} else {
		inputBytes = data.Input
	}

	// replay the step run
	_, err = ec.repo.StepRun().ReplayStepRun(
		ctx,
		metadata.TenantId,
		sqlchelpers.UUIDToStr(stepRun.SRID),
		inputBytes,
	)

	if err != nil {
		return fmt.Errorf("could not update step run for replay: %w", err)
	}

	return ec.queueStepRun(ctx, metadata.TenantId, sqlchelpers.UUIDToStr(stepRun.StepId), sqlchelpers.UUIDToStr(stepRun.SRID), true)
}

func (ec *JobsControllerImpl) handleStepRunQueued(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-queued", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunTaskPayload{}
	metadata := tasktypes.StepRunTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode job task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode job task metadata: %w", err)
	}

	isRetry := false

	if payload.RetryCount != nil && *payload.RetryCount > 0 {
		isRetry = true
	}

	return ec.queueStepRun(ctx, metadata.TenantId, metadata.StepId, payload.StepRunId, isRetry)
}

func (jc *JobsControllerImpl) runPgStat() func() {
	return func() {
		s := jc.repo.Health().PgStat()

		jc.pgxStatsLogger.Info().Int32(
			"total_connections", s.TotalConns(),
		).Int32(
			"constructing_connections", s.ConstructingConns(),
		).Int64(
			"acquired_connections", s.AcquireCount(),
		).Int32(
			"idle_connections", s.IdleConns(),
		).Int32(
			"max_connections", s.MaxConns(),
		).Dur(
			"acquire_duration", s.AcquireDuration(),
		).Int64(
			"empty_acquire_count", s.EmptyAcquireCount(),
		).Int64(
			"canceled_acquire_count", s.CanceledAcquireCount(),
		).Msg("pgx stats")
	}
}

func MakeBatched[T any](batchSize int, things []T, fn func(group []T) error) error {
	g := new(errgroup.Group)

	for i := 0; i < len(things); i += batchSize {
		end := i + batchSize
		if end > len(things) {
			end = len(things)
		}

		group := things[i:end]

		g.Go(func() error {
			return fn(group)
		})
	}

	return g.Wait()
}

func (jc *JobsControllerImpl) runStepRunReassign(ctx context.Context, startedAt time.Time) func() {
	return func() {
		// if we are within 15 seconds of the started time, then we should not reassign step runs
		if time.Since(startedAt) < 15*time.Second {
			return
		}

		// we set requeueAfter to 4 seconds in the future to avoid requeuing the same step run multiple times
		ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		defer cancel()

		jc.l.Debug().Msgf("jobs controller: checking step run reassignment")

		// list all tenants
		tenants, err := jc.repo.Tenant().ListTenantsByControllerPartition(ctx, jc.p.GetControllerPartitionId())

		if err != nil {
			jc.l.Err(err).Msg("could not list tenants")
			return
		}

		g := new(errgroup.Group)

		for i := range tenants {
			tenantId := sqlchelpers.UUIDToStr(tenants[i].ID)

			g.Go(func() error {
				return jc.runStepRunReassignTenant(ctx, tenantId)
			})
		}

		err = g.Wait()

		if err != nil {
			jc.l.Err(err).Msg("could not run step run reassign")
		}
	}
}

// runStepRunReassignTenant looks for step runs that have been assigned to a worker but have not started,
// or have been running but the worker has become inactive.
func (ec *JobsControllerImpl) runStepRunReassignTenant(ctx context.Context, tenantId string) error {
	// we want only one requeue running at a time for a tenant
	if _, ok := ec.reassignMutexes.Load(tenantId); !ok {
		ec.reassignMutexes.Store(tenantId, &sync.Mutex{})
	}

	muInt, _ := ec.reassignMutexes.Load(tenantId)
	mu := muInt.(*sync.Mutex)

	if !mu.TryLock() {
		return nil
	}

	defer mu.Unlock()

	ctx, span := telemetry.NewSpan(ctx, "handle-step-run-reassign")
	defer span.End()

	_, err := ec.repo.StepRun().ListStepRunsToReassign(ctx, tenantId)

	if err != nil {
		return fmt.Errorf("could not list step runs to reassign for tenant %s: %w", tenantId, err)
	}

	return nil
}

func (jc *JobsControllerImpl) runStepRunTimeout(ctx context.Context) func() {
	return func() {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		jc.l.Debug().Msgf("jobs controller: running step run timeout")

		// list all tenants
		tenants, err := jc.repo.Tenant().ListTenantsByControllerPartition(ctx, jc.p.GetControllerPartitionId())

		if err != nil {
			jc.l.Err(err).Msg("could not list tenants")
			return
		}

		g := new(errgroup.Group)

		for i := range tenants {
			tenantId := sqlchelpers.UUIDToStr(tenants[i].ID)

			g.Go(func() error {
				return jc.runStepRunTimeoutTenant(ctx, tenantId)
			})
		}

		err = g.Wait()

		if err != nil {
			jc.l.Err(err).Msg("could not run step run timeout")
		}
	}
}

// runStepRunTimeoutTenant looks for step runs that are timed out in the tenant.
func (ec *JobsControllerImpl) runStepRunTimeoutTenant(ctx context.Context, tenantId string) error {
	// we want only one requeue running at a time for a tenant
	if _, ok := ec.timeoutMutexes.Load(tenantId); !ok {
		ec.timeoutMutexes.Store(tenantId, &sync.Mutex{})
	}

	muInt, _ := ec.timeoutMutexes.Load(tenantId)
	mu := muInt.(*sync.Mutex)

	if !mu.TryLock() {
		return nil
	}

	defer mu.Unlock()

	ctx, span := telemetry.NewSpan(ctx, "handle-step-run-timeout")
	defer span.End()

	stepRuns, err := ec.repo.StepRun().ListStepRunsToTimeout(ctx, tenantId)

	if err != nil {
		return fmt.Errorf("could not list step runs to timeout for tenant %s: %w", tenantId, err)
	}

	if num := len(stepRuns); num > 0 {
		ec.l.Info().Msgf("timing out %d step runs", num)
	}

	return MakeBatched(10, stepRuns, func(group []*dbsqlc.GetStepRunForEngineRow) error {
		scheduleCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()

		scheduleCtx, span := telemetry.NewSpan(scheduleCtx, "handle-step-run-timeout-step-run")
		defer span.End()

		for i := range group {
			stepRunCp := group[i]

			defer span.End()

			stepRunId := sqlchelpers.UUIDToStr(stepRunCp.SRID)

			err = ec.failStepRun(scheduleCtx, tenantId, stepRunId, "TIMED_OUT", time.Now().UTC())
			if err != nil {
				return err
			}
		}

		return nil
	})
}

func (ec *JobsControllerImpl) queueStepRun(ctx context.Context, tenantId, stepId, stepRunId string, isRetry bool) error {
	ctx, span := telemetry.NewSpan(ctx, "queue-step-run")
	defer span.End()

	// add the rendered data to the step run
	stepRun, err := ec.repo.StepRun().GetStepRunForEngine(ctx, tenantId, stepRunId)

	errData := map[string]interface{}{
		"tenant_id":   tenantId,
		"step_id":     stepId,
		"step_run_id": stepRunId,
	}

	if err != nil {
		return ec.a.WrapErr(fmt.Errorf("could not get step run: %w", err), errData)
	}

	data, err := ec.repo.StepRun().GetStepRunDataForEngine(ctx, tenantId, stepRunId)

	if err != nil {
		return ec.a.WrapErr(fmt.Errorf("could not get step run data: %w", err), errData)
	}

	servertel.WithStepRunModel(span, stepRun)

	queueOpts := &repository.QueueStepRunOpts{
		IsRetry: isRetry,
	}

	// If the step run input is not set, then we should set it. This will be set upstream if we've rerun
	// the step run manually with new inputs. It will not be set when the step is automatically queued.
	if in := data.Input; len(in) == 0 || string(in) == "{}" {
		lookupDataBytes := data.JobRunLookupData

		if lookupDataBytes != nil {
			lookupData := &datautils.JobRunLookupData{}

			err := json.Unmarshal(lookupDataBytes, lookupData)

			if err != nil {
				return ec.a.WrapErr(fmt.Errorf("could not get job run lookup data: %w", err), errData)
			}

			userData := map[string]interface{}{}

			if setUserData := stepRun.StepCustomUserData; len(setUserData) > 0 {
				err := json.Unmarshal(setUserData, &userData)

				if err != nil {
					return fmt.Errorf("could not unmarshal custom user data: %w", err)
				}
			}

			// input data is the triggering event data and any parent step data
			inputData := datautils.StepRunData{
				Input:       lookupData.Input,
				TriggeredBy: lookupData.TriggeredBy,
				Parents:     lookupData.Steps,
				UserData:    userData,
				Overrides:   map[string]interface{}{},
			}

			inputDataBytes, err := json.Marshal(inputData)

			if err != nil {
				return ec.a.WrapErr(fmt.Errorf("could not convert input data to json: %w", err), errData)
			}

			queueOpts.Input = inputDataBytes
		}
	}

	// indicate that the step run is pending assignment
	_, err = ec.repo.StepRun().QueueStepRun(ctx, tenantId, stepRunId, queueOpts)

	if err != nil {
		if errors.Is(err, repository.ErrStepRunIsNotPending) {
			ec.l.Debug().Msgf("step run %s is not pending, skipping scheduling", stepRunId)
			return nil
		}

		return ec.a.WrapErr(fmt.Errorf("could not update step run: %w", err), errData)
	}

	defer ec.checkTenantQueue(ctx, tenantId)

	return nil
}

func (ec *JobsControllerImpl) checkTenantQueue(ctx context.Context, tenantId string) {
	// send a message to the tenant partition queue that a step run is ready to be scheduled
	tenant, err := ec.repo.Tenant().GetTenantByID(ctx, tenantId)

	if err != nil {
		ec.l.Err(err).Msg("could not add message to tenant partition queue")
		return
	}

	if tenant.ControllerPartitionId.Valid {
		err = ec.mq.AddMessage(
			ctx,
			msgqueue.QueueTypeFromPartitionID(tenant.ControllerPartitionId.String),
			tasktypes.CheckTenantQueueToTask(tenantId),
		)

		if err != nil {
			ec.l.Err(err).Msg("could not add message to tenant partition queue")
		}
	}
}

func (ec *JobsControllerImpl) handleStepRunStarted(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-started", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunStartedTaskPayload{}
	metadata := tasktypes.StepRunStartedTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode step run started task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode step run started task metadata: %w", err)
	}

	// update the step run in the database
	startedAt, err := time.Parse(time.RFC3339, payload.StartedAt)

	if err != nil {
		return fmt.Errorf("could not parse started at: %w", err)
	}

	err = ec.repo.StepRun().StepRunStarted(ctx, metadata.TenantId, payload.StepRunId, startedAt)

	if err != nil {
		return fmt.Errorf("could not update step run: %w", err)
	}

	return nil
}

func (ec *JobsControllerImpl) handleStepRunFinished(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-finished", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunFinishedTaskPayload{}
	metadata := tasktypes.StepRunFinishedTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode step run finished task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode step run finished task metadata: %w", err)
	}

	// update the step run in the database
	finishedAt, err := time.Parse(time.RFC3339, payload.FinishedAt)

	if err != nil {
		return fmt.Errorf("could not parse started at: %w", err)
	}

	var stepOutput []byte

	if payload.StepOutputData != "" {
		stepOutput = []byte(payload.StepOutputData)
	}

	err = ec.repo.StepRun().StepRunSucceeded(ctx, metadata.TenantId, payload.StepRunId, finishedAt, stepOutput)

	if err != nil {
		return fmt.Errorf("could not update step run: %w", err)
	}

	// recheck the tenant queue
	ec.checkTenantQueue(ctx, metadata.TenantId)

	return nil
}

func (ec *JobsControllerImpl) handleStepRunFailed(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-failed", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunFailedTaskPayload{}
	metadata := tasktypes.StepRunFailedTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode step run failed task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode step run failed task metadata: %w", err)
	}

	failedAt, err := time.Parse(time.RFC3339, payload.FailedAt)
	if err != nil {
		return fmt.Errorf("could not parse failed at: %w", err)
	}

	return ec.failStepRun(ctx, metadata.TenantId, payload.StepRunId, payload.Error, failedAt)
}

func (ec *JobsControllerImpl) failStepRun(ctx context.Context, tenantId, stepRunId, errorReason string, failedAt time.Time) error {
	stepRun, err := ec.repo.StepRun().GetStepRunForEngine(ctx, tenantId, stepRunId)

	if err != nil {
		return fmt.Errorf("could not get step run: %w", err)
	}

	// check the queue on failure
	defer ec.checkTenantQueue(ctx, tenantId)

	// determine if step run should be retried or not
	shouldRetry := stepRun.SRRetryCount < stepRun.StepRetries

	if shouldRetry {
		eventMessage := fmt.Sprintf("Step run failed on %s", failedAt.Format(time.RFC1123))

		eventReason := dbsqlc.StepRunEventReasonFAILED

		if errorReason == "TIMED_OUT" {
			eventReason = dbsqlc.StepRunEventReasonTIMEDOUT
			eventMessage = fmt.Sprintf("Step exceeded timeout duration (%s)", stepRun.StepTimeout.String)
		}

		eventMessage += ", and will be retried."

		defer ec.repo.StepRun().DeferredStepRunEvent(tenantId, stepRunId, repository.CreateStepRunEventOpts{
			EventReason:   repository.StepRunEventReasonPtr(eventReason),
			EventMessage:  repository.StringPtr(eventMessage),
			EventSeverity: repository.StepRunEventSeverityPtr(dbsqlc.StepRunEventSeverityCRITICAL),
			EventData: map[string]interface{}{
				"retry_count": stepRun.SRRetryCount,
			},
		})

		// send a task to the taskqueue
		return ec.mq.AddMessage(
			ctx,
			msgqueue.JOB_PROCESSING_QUEUE,
			tasktypes.StepRunRetryToTask(stepRun, nil, errorReason),
		)
	}

	// fail step run
	err = ec.repo.StepRun().StepRunFailed(ctx, tenantId, stepRunId, failedAt, errorReason)

	if err != nil {
		return fmt.Errorf("could not fail step run: %w", err)
	}

	attemptCancel := false

	if errorReason == "TIMED_OUT" {
		attemptCancel = true
	}

	if !stepRun.SRWorkerId.Valid {
		// this is not a fatal error
		ec.l.Warn().Msgf("[failStepRun] step run %s has no worker id, skipping cancellation", stepRunId)
		attemptCancel = false
	} else {
		ec.l.Info().Msgf("[failStepRun] step run %s has a worker id, cancelling", stepRunId)
	}

	// Attempt to cancel the previous running step run
	if attemptCancel {
		workerId := sqlchelpers.UUIDToStr(stepRun.SRWorkerId)

		worker, err := ec.repo.Worker().GetWorkerForEngine(ctx, tenantId, workerId)

		if err != nil {
			return fmt.Errorf("could not get worker: %w", err)
		} else if !worker.DispatcherId.Valid {
			return fmt.Errorf("worker has no dispatcher id")
		}

		dispatcherId := sqlchelpers.UUIDToStr(worker.DispatcherId)

		err = ec.mq.AddMessage(
			ctx,
			msgqueue.QueueTypeFromDispatcherID(dispatcherId),
			stepRunCancelledTask(
				tenantId,
				stepRunId,
				workerId,
				dispatcherId,
				errorReason,
				sqlchelpers.UUIDToStr(stepRun.WorkflowRunId),
				&stepRun.StepRetries,
				&stepRun.SRRetryCount,
			),
		)

		if err != nil {
			return fmt.Errorf("could not add job assigned task to task queue: %w", err)
		}
	}

	return nil
}

func (ec *JobsControllerImpl) handleStepRunTimedOut(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-timed-out", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunTimedOutTaskPayload{}
	metadata := tasktypes.StepRunTimedOutTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode step run timed out task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode step run timed out task metadata: %w", err)
	}

	return ec.failStepRun(ctx, metadata.TenantId, payload.StepRunId, "TIMED_OUT", time.Now().UTC())
}

func (ec *JobsControllerImpl) handleStepRunCancel(ctx context.Context, task *msgqueue.Message) error {
	ctx, span := telemetry.NewSpanWithCarrier(ctx, "handle-step-run-cancel", task.OtelCarrier)
	defer span.End()

	payload := tasktypes.StepRunCancelTaskPayload{}
	metadata := tasktypes.StepRunCancelTaskMetadata{}

	err := ec.dv.DecodeAndValidate(task.Payload, &payload)

	if err != nil {
		return fmt.Errorf("could not decode step run notify cancel task payload: %w", err)
	}

	err = ec.dv.DecodeAndValidate(task.Metadata, &metadata)

	if err != nil {
		return fmt.Errorf("could not decode step run notify cancel task metadata: %w", err)
	}

	return ec.cancelStepRun(ctx, metadata.TenantId, payload.StepRunId, payload.CancelledReason)
}

func (ec *JobsControllerImpl) cancelStepRun(ctx context.Context, tenantId, stepRunId, reason string) error {
	ctx, span := telemetry.NewSpan(ctx, "cancel-step-run")
	defer span.End()

	// cancel current step run
	now := time.Now().UTC()

	// get the old step run to figure out the worker and dispatcher id, before we update the step run
	oldStepRun, err := ec.repo.StepRun().GetStepRunForEngine(ctx, tenantId, stepRunId)

	if err != nil {
		return fmt.Errorf("could not get step run: %w", err)
	}

	err = ec.repo.StepRun().StepRunCancelled(ctx, tenantId, stepRunId, now, reason)

	if err != nil {
		return fmt.Errorf("could not cancel step run: %w", err)
	}

	if !oldStepRun.SRWorkerId.Valid {
		// this is not a fatal error
		ec.l.Warn().Msgf("[cancelStepRun] step run %s has no worker id, skipping send of cancellation", stepRunId)

		return nil
	}

	ec.l.Info().Msgf("[cancelStepRun] step run %s has a worker id, sending cancellation", stepRunId)

	workerId := sqlchelpers.UUIDToStr(oldStepRun.SRWorkerId)

	worker, err := ec.repo.Worker().GetWorkerForEngine(ctx, tenantId, workerId)

	if err != nil {
		return fmt.Errorf("could not get worker: %w", err)
	} else if !worker.DispatcherId.Valid {
		return fmt.Errorf("worker has no dispatcher id")
	}

	dispatcherId := sqlchelpers.UUIDToStr(worker.DispatcherId)

	err = ec.mq.AddMessage(
		ctx,
		msgqueue.QueueTypeFromDispatcherID(dispatcherId),
		stepRunCancelledTask(tenantId, stepRunId, workerId, dispatcherId, reason,
			sqlchelpers.UUIDToStr(oldStepRun.WorkflowRunId), &oldStepRun.StepRetries, &oldStepRun.SRRetryCount,
		),
	)

	if err != nil {
		return fmt.Errorf("could not add job assigned task to task queue: %w", err)
	}

	return nil
}

func stepRunAssignedTask(tenantId, stepRunId, workerId, dispatcherId string) *msgqueue.Message {
	payload, _ := datautils.ToJSONMap(tasktypes.StepRunAssignedTaskPayload{
		StepRunId: stepRunId,
		WorkerId:  workerId,
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.StepRunAssignedTaskMetadata{
		TenantId:     tenantId,
		DispatcherId: dispatcherId,
	})

	return &msgqueue.Message{
		ID:       "step-run-assigned",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}
}

func stepRunCancelledTask(tenantId, stepRunId, workerId, dispatcherId, cancelledReason string, runId string, retries *int32, retryCount *int32) *msgqueue.Message {
	payload, _ := datautils.ToJSONMap(tasktypes.StepRunCancelledTaskPayload{
		WorkflowRunId:   runId,
		StepRunId:       stepRunId,
		WorkerId:        workerId,
		CancelledReason: cancelledReason,
		StepRetries:     retries,
		RetryCount:      retryCount,
	})

	metadata, _ := datautils.ToJSONMap(tasktypes.StepRunCancelledTaskMetadata{
		TenantId:     tenantId,
		DispatcherId: dispatcherId,
	})

	// TODO add additional metadata
	return &msgqueue.Message{
		ID:       "step-run-cancelled",
		Payload:  payload,
		Metadata: metadata,
		Retries:  3,
	}
}
