package firedb

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bufbuild/connect-go"
	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/multierror"
	"github.com/grafana/dskit/services"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/samber/lo"
	"golang.org/x/sync/errgroup"

	"github.com/grafana/fire/pkg/firedb/block"
	commonv1 "github.com/grafana/fire/pkg/gen/common/v1"
	ingestv1 "github.com/grafana/fire/pkg/gen/ingester/v1"
	firemodel "github.com/grafana/fire/pkg/model"
	"github.com/grafana/fire/pkg/objstore/client"
	"github.com/grafana/fire/pkg/objstore/providers/filesystem"
)

type Config struct {
	DataPath      string
	BlockDuration time.Duration
}

func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.StringVar(&cfg.DataPath, "firedb.data-path", "./data", "Directory used for local storage.")
	f.DurationVar(&cfg.BlockDuration, "firedb.block-duration", 30*time.Minute, "Block duration.")
}

type FireDB struct {
	services.Service

	cfg    *Config
	reg    prometheus.Registerer
	logger log.Logger
	stopCh chan struct{}

	headLock      sync.RWMutex
	head          *Head
	headMetrics   *headMetrics
	headFlushTime time.Time

	blockQuerier *BlockQuerier
}

func New(cfg *Config, logger log.Logger, reg prometheus.Registerer) (*FireDB, error) {
	headMetrics := newHeadMetrics(reg)
	f := &FireDB{
		cfg:         cfg,
		reg:         reg,
		logger:      logger,
		stopCh:      make(chan struct{}, 0),
		headMetrics: headMetrics,
	}
	if _, err := f.initHead(); err != nil {
		return nil, err
	}
	f.Service = services.NewBasicService(f.starting, f.running, f.stopping)

	fs, err := filesystem.NewBucket(cfg.DataPath)
	if err != nil {
		return nil, err
	}
	bucketReader, err := client.ReaderAtBucket(pathLocal, fs, prometheus.WrapRegistererWithPrefix("firedb", reg))
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(f.LocalDataPath(), 0o777); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", f.LocalDataPath(), err)
	}

	f.blockQuerier = NewBlockQuerier(logger, bucketReader)

	// do an initial querier sync
	ctx := context.Background()
	if err := f.blockQuerier.Sync(ctx); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *FireDB) LocalDataPath() string {
	return filepath.Join(f.cfg.DataPath, pathLocal)
}

func (f *FireDB) BlockMetas(ctx context.Context) ([]*block.Meta, error) {
	return f.blockQuerier.BlockMetas(ctx)
}

func (f *FireDB) runBlockQuerierSync(ctx context.Context) {
	if err := f.blockQuerier.Sync(ctx); err != nil {
		level.Error(f.logger).Log("msg", "sync blocks failed", "err", err)
	}
}

func (f *FireDB) loop() {
	var (
		blockScanTicker = time.NewTicker(5 * time.Minute)
		blockScanManual = make(chan struct{}, 1)
	)
	defer func() {
		close(blockScanManual)
		blockScanTicker.Stop()
	}()

	for {
		ctx := context.Background()

		f.headLock.RLock()
		timeToFlush := f.headFlushTime.Sub(time.Now())
		f.headLock.RUnlock()

		select {
		case <-f.stopCh:
			return
		case <-time.After(timeToFlush):
			if err := f.Flush(ctx); err != nil {
				level.Error(f.logger).Log("msg", "flushing head block failed", "err", err)
				continue
			}
			f.runBlockQuerierSync(ctx)
		case <-blockScanTicker.C:
			f.runBlockQuerierSync(ctx)
		}
	}
}

func (f *FireDB) starting(ctx context.Context) error {
	go f.loop()
	return nil
}

func (f *FireDB) running(ctx context.Context) error {
	select {
	// wait until service is asked to stop
	case <-ctx.Done():
		// stop
		close(f.stopCh)
	}
	return nil
}

func (f *FireDB) stopping(_ error) error {
	errs := multierror.New()
	if err := f.blockQuerier.Close(); err != nil {
		errs.Add(err)
	}
	if err := f.Close(context.Background()); err != nil {
		errs.Add(err)
	}
	return errs.Err()
}

func (f *FireDB) Head() *Head {
	f.headLock.RLock()
	defer f.headLock.RUnlock()
	return f.head
}

type Queriers []Querier

func (qs Queriers) SelectProfiles(ctx context.Context, req *connect.Request[ingestv1.SelectProfilesRequest]) (*connect.Response[ingestv1.SelectProfilesResponse], error) {
	// first check which profileSelecters are in range before executing
	qs = lo.Filter(qs, func(e Querier, _ int) bool {
		return e.InRange(
			model.Time(req.Msg.Start),
			model.Time(req.Msg.End),
		)
	})

	results := make([]*ingestv1.SelectProfilesResponse, len(qs))

	g, ctx := errgroup.WithContext(ctx)
	// todo not sure this help on disk IO
	g.SetLimit(16)

	query := func(ctx context.Context, pos int) {
		g.Go(func() error {
			resp, err := qs[pos].SelectProfiles(ctx, req)
			if err != nil {
				return err
			}

			results[pos] = resp.Msg
			return nil
		})
	}

	for pos := range qs {
		query(ctx, pos)
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	return connect.NewResponse(mergeSelectProfilesResponse(results...)), nil
}

func mergeSelectProfilesResponse(responses ...*ingestv1.SelectProfilesResponse) *ingestv1.SelectProfilesResponse {
	var (
		result    *ingestv1.SelectProfilesResponse
		posByName map[string]int32
	)

	for _, resp := range responses {
		// skip empty results
		if resp == nil || len(resp.Profiles) == 0 {
			continue
		}

		// first non-empty result result
		if result == nil {
			result = resp
			continue
		}

		// build up the lookup map the first time
		if posByName == nil {
			posByName = make(map[string]int32)
			for idx, n := range result.FunctionNames {
				posByName[n] = int32(idx)
			}
		}

		// lookup and add missing functionNames
		var (
			rewrite = make([]int32, len(resp.FunctionNames))
			ok      bool
		)
		for idx, n := range resp.FunctionNames {
			rewrite[idx], ok = posByName[n]
			if ok {
				continue
			}

			// need to add functionName to list
			rewrite[idx] = int32(len(result.FunctionNames))
			result.FunctionNames = append(result.FunctionNames, n)
		}

		// rewrite existing function ids, by building a list of unique slices
		functionIDsUniq := make(map[*int32][]int32)
		for _, profile := range resp.Profiles {
			for _, sample := range profile.Stacktraces {
				if len(sample.FunctionIds) == 0 {
					continue
				}
				functionIDsUniq[&sample.FunctionIds[0]] = sample.FunctionIds
			}
		}
		// now rewrite those ids in slices
		for _, slice := range functionIDsUniq {
			for idx, functionID := range slice {
				slice[idx] = rewrite[functionID]
			}
		}
		result.Profiles = append(result.Profiles, resp.Profiles...)
	}

	// ensure nil will always be the empty response
	if result == nil {
		result = &ingestv1.SelectProfilesResponse{}
	}

	return result
}

func (f *FireDB) SelectProfiles(ctx context.Context, req *connect.Request[ingestv1.SelectProfilesRequest]) (*connect.Response[ingestv1.SelectProfilesResponse], error) {
	return f.querierFor(model.Time(req.Msg.Start), model.Time(req.Msg.End)).SelectProfiles(ctx, req)
}

func (f *FireDB) querierFor(start, end model.Time) Queriers {
	blocks := f.blockQuerier.queriersFor(start, end)
	if f.Head().InRange(start, end) {
		res := make(Queriers, 0, len(blocks)+1)
		res = append(res, f.Head())
		res = append(res, blocks...)
		return res
	}
	return blocks
}

func (f *FireDB) MergeProfilesStacktraces(ctx context.Context, stream *connect.BidiStream[ingestv1.MergeProfilesStacktracesRequest, ingestv1.MergeProfilesStacktracesResponse]) error {
	r, err := stream.Receive()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return connect.NewError(connect.CodeCanceled, errors.New("client closed stream"))
		}
		return err
	}
	fmt.Println("receive first request")

	if r.Request == nil {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("missing initial select request"))
	}
	request := r.Request

	batchSize := 2048

	type labelWithIndex struct {
		firemodel.Labels
		index int
	}

	queriers := f.querierFor(model.Time(request.Start), model.Time(request.End))

	selectProfileResult := &ingestv1.ProfileSets{
		Profiles:   make([]*ingestv1.SeriesProfile, 0, batchSize),
		LabelsSets: make([]*commonv1.Labels, 0, batchSize),
	}
	result := make([]*ingestv1.MergeProfilesStacktracesResult, 0, len(queriers))

	for i, q := range queriers {
		fmt.Println("starting merge q:", i)

		res, err := q.BatchMergeStacktraces(ctx, request, batchSize, func(selectedProfiles []Profile) (Keep, error) {
			seriesByFP := map[model.Fingerprint]labelWithIndex{}
			selectProfileResult.LabelsSets = selectProfileResult.LabelsSets[:0]
			selectProfileResult.Profiles = selectProfileResult.Profiles[:0]

			for _, profile := range selectedProfiles {
				var ok bool
				var lblsIdx labelWithIndex
				lblsIdx, ok = seriesByFP[profile.Fingerprint()]
				if !ok {
					lblsIdx = labelWithIndex{
						Labels: profile.Labels(),
						index:  len(selectProfileResult.LabelsSets),
					}
					seriesByFP[profile.Fingerprint()] = lblsIdx
					selectProfileResult.LabelsSets = append(selectProfileResult.LabelsSets, &commonv1.Labels{Labels: profile.Labels()})
				}
				selectProfileResult.Profiles = append(selectProfileResult.Profiles, &ingestv1.SeriesProfile{
					LabelIndex: int32(lblsIdx.index),
					Timestamp:  int64(profile.Timestamp()),
				})

			}
			fmt.Println("sending potential profiles:", len(selectProfileResult.Profiles))

			// read a batch of profiles and sends it.
			err := stream.Send(&ingestv1.MergeProfilesStacktracesResponse{
				SelectedProfiles: selectProfileResult,
			})
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, connect.NewError(connect.CodeCanceled, errors.New("client closed stream"))
				}
				return nil, err
			}
			fmt.Println("waiting selection of profiles:", len(selectProfileResult.Profiles))

			// handle response for the batch.
			selectionResponse, err := stream.Receive()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil, connect.NewError(connect.CodeCanceled, errors.New("client closed stream"))
				}
				return nil, err
			}
			fmt.Println("received selection of profiles:", len(selectionResponse.Profiles))

			return Keep(selectionResponse.Profiles), nil
		})
		if err != nil {
			return err
		}
		result = append(result, res)
	}

	err = stream.Send(&ingestv1.MergeProfilesStacktracesResponse{
		Result: firemodel.MergeBatchMergeStacktraces(result...),
	})
	if err != nil {
		if errors.Is(err, io.EOF) {
			return connect.NewError(connect.CodeCanceled, errors.New("client closed stream"))
		}
		return err
	}

	return nil
}

func (f *FireDB) initHead() (oldHead *Head, err error) {
	f.headLock.Lock()
	defer f.headLock.Unlock()
	oldHead = f.head
	f.headFlushTime = time.Now().UTC().Truncate(f.cfg.BlockDuration).Add(f.cfg.BlockDuration)
	f.head, err = NewHead(f.cfg.DataPath, headWithMetrics(f.headMetrics), HeadWithLogger(f.logger))
	if err != nil {
		return oldHead, err
	}
	return oldHead, nil
}

func (f *FireDB) Flush(ctx context.Context) error {
	oldHead, err := f.initHead()
	if err != nil {
		return err
	}

	if oldHead == nil {
		return nil
	}
	return oldHead.Flush(ctx)
}

func (f *FireDB) Close(ctx context.Context) error {
	return f.head.Flush(ctx)
}
