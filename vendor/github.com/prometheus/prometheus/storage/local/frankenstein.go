// Copyright 2016 The Prometheus Authors

package local

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	frank "github.com/weaveworks/frankenstein/chunk"
	"github.com/weaveworks/frankenstein/user"
	"golang.org/x/net/context"

	"github.com/prometheus/prometheus/storage/metric"
)

const (
	ingesterSubsystem        = "ingester"
	maxConcurrentFlushSeries = 100
)

var (
	memorySeriesDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, ingesterSubsystem, "memory_series"),
		"The current number of series in memory.",
		nil, nil,
	)
	memoryUsersDesc = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, ingesterSubsystem, "memory_users"),
		"The current number of users in memory.",
		nil, nil,
	)
)

// Ingester deals with "in flight" chunks.
// Its like MemorySeriesStorage, but simpler.
type Ingester struct {
	cfg                IngesterConfig
	chunkStore         frank.Store
	stopLock           sync.RWMutex
	stopped            bool
	quit               chan struct{}
	done               chan struct{}
	flushSeriesLimiter frank.Semaphore

	userStateLock sync.Mutex
	userState     map[string]*userState

	ingestedSamples    prometheus.Counter
	discardedSamples   *prometheus.CounterVec
	chunkUtilization   prometheus.Histogram
	chunkStoreFailures prometheus.Counter
	queries            prometheus.Counter
	queriedSamples     prometheus.Counter
	memoryChunks       prometheus.Gauge
}

type IngesterConfig struct {
	FlushCheckPeriod time.Duration
	MaxChunkAge      time.Duration
}

type userState struct {
	userID     string
	fpLocker   *fingerprintLocker
	fpToSeries *seriesMap
	mapper     *fpMapper
	index      *invertedIndex
}

func NewIngester(cfg IngesterConfig, chunkStore frank.Store) (*Ingester, error) {
	if cfg.FlushCheckPeriod == 0 {
		cfg.FlushCheckPeriod = 1 * time.Minute
	}
	if cfg.MaxChunkAge == 0 {
		cfg.MaxChunkAge = 10 * time.Minute
	}

	i := &Ingester{
		cfg:                cfg,
		chunkStore:         chunkStore,
		quit:               make(chan struct{}),
		done:               make(chan struct{}),
		flushSeriesLimiter: frank.NewSemaphore(maxConcurrentFlushSeries),

		userState: map[string]*userState{},

		ingestedSamples: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "ingested_samples_total",
			Help:      "The total number of samples ingested.",
		}),
		discardedSamples: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: ingesterSubsystem,
				Name:      "out_of_order_samples_total",
				Help:      "The total number of samples that were discarded because their timestamps were at or before the last received sample for a series.",
			},
			[]string{discardReasonLabel},
		),
		chunkUtilization: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "chunk_utilization",
			Help:      "Distribution of stored chunk utilization.",
			Buckets:   []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9},
		}),
		memoryChunks: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "memory_chunks",
			Help:      "The total number of samples returned from queries.",
		}),
		chunkStoreFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "chunk_store_failures_total",
			Help:      "The total number of errors while storing chunks to the chunk store.",
		}),
		queries: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "queries_total",
			Help:      "The total number of queries the ingester has handled.",
		}),
		queriedSamples: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: ingesterSubsystem,
			Name:      "queried_samples_total",
			Help:      "The total number of samples returned from queries.",
		}),
	}

	go i.loop()
	return i, nil
}

func (i *Ingester) getStateFor(ctx context.Context) (*userState, error) {
	userID, err := user.GetID(ctx)
	if err != nil {
		return nil, fmt.Errorf("no user id")
	}

	i.userStateLock.Lock()
	defer i.userStateLock.Unlock()
	state, ok := i.userState[userID]
	if !ok {
		state = &userState{
			userID:     userID,
			fpToSeries: newSeriesMap(),
			fpLocker:   newFingerprintLocker(16),
			index:      newInvertedIndex(),
		}
		var err error
		state.mapper, err = newFPMapper(state.fpToSeries, noopPersistence{})
		if err != nil {
			return nil, err
		}
		i.userState[userID] = state
	}
	return state, nil
}

func (*Ingester) NeedsThrottling(_ context.Context) bool {
	return false
}

func (i *Ingester) Append(ctx context.Context, samples []*model.Sample) error {
	for _, sample := range samples {
		if err := i.append(ctx, sample); err != nil {
			return err
		}
	}
	return nil
}

func (i *Ingester) append(ctx context.Context, sample *model.Sample) error {
	for ln, lv := range sample.Metric {
		if len(lv) == 0 {
			delete(sample.Metric, ln)
		}
	}

	i.stopLock.RLock()
	defer i.stopLock.RUnlock()
	if i.stopped {
		return fmt.Errorf("ingester stopping")
	}

	state, err := i.getStateFor(ctx)
	if err != nil {
		return err
	}

	fp, series, err := state.getOrCreateSeries(sample.Metric)
	if err != nil {
		return err
	}
	defer func() {
		state.fpLocker.Unlock(fp)
	}()

	if sample.Timestamp == series.lastTime {
		// Don't report "no-op appends", i.e. where timestamp and sample
		// value are the same as for the last append, as they are a
		// common occurrence when using client-side timestamps
		// (e.g. Pushgateway or federation).
		if sample.Timestamp == series.lastTime &&
			series.lastSampleValueSet &&
			sample.Value.Equal(series.lastSampleValue) {
			return nil
		}
		i.discardedSamples.WithLabelValues(duplicateSample).Inc()
		return ErrDuplicateSampleForTimestamp // Caused by the caller.
	}
	if sample.Timestamp < series.lastTime {
		i.discardedSamples.WithLabelValues(outOfOrderTimestamp).Inc()
		return ErrOutOfOrderSample // Caused by the caller.
	}
	prevNumChunks := len(series.chunkDescs)
	_, err = series.add(model.SamplePair{
		Value:     sample.Value,
		Timestamp: sample.Timestamp,
	})
	i.memoryChunks.Add(float64(len(series.chunkDescs) - prevNumChunks))

	if err == nil {
		// TODO: Track append failures too (unlikely to happen).
		i.ingestedSamples.Inc()
	}
	return err
}

func (u *userState) getOrCreateSeries(metric model.Metric) (model.Fingerprint, *memorySeries, error) {
	rawFP := metric.FastFingerprint()
	u.fpLocker.Lock(rawFP)
	fp := u.mapper.mapFP(rawFP, metric)
	if fp != rawFP {
		u.fpLocker.Unlock(rawFP)
		u.fpLocker.Lock(fp)
	}

	series, ok := u.fpToSeries.get(fp)
	if ok {
		return fp, series, nil
	}

	var err error
	series, err = newMemorySeries(metric, nil, time.Time{})
	if err != nil {
		// err should always be nil when chunkDescs are nil
		panic(err)
	}
	u.fpToSeries.put(fp, series)
	u.index.add(metric, fp)
	return fp, series, nil
}

func (i *Ingester) Query(ctx context.Context, from, through model.Time, matchers ...*metric.LabelMatcher) (model.Matrix, error) {
	i.queries.Inc()

	state, err := i.getStateFor(ctx)
	if err != nil {
		return nil, err
	}

	fps := state.index.lookup(matchers)

	// fps is sorted, lock them in order to prevent deadlocks
	queriedSamples := 0
	result := model.Matrix{}
	for _, fp := range fps {
		state.fpLocker.Lock(fp)
		series, ok := state.fpToSeries.get(fp)
		if !ok {
			state.fpLocker.Unlock(fp)
			continue
		}

		values, err := samplesForRange(series, from, through)
		state.fpLocker.Unlock(fp)
		if err != nil {
			return nil, err
		}

		result = append(result, &model.SampleStream{
			Metric: series.metric,
			Values: values,
		})
		queriedSamples += len(values)
	}

	i.queriedSamples.Add(float64(queriedSamples))

	return result, nil
}

func samplesForRange(s *memorySeries, from, through model.Time) ([]model.SamplePair, error) {
	// Find first chunk with start time after "from".
	fromIdx := sort.Search(len(s.chunkDescs), func(i int) bool {
		return s.chunkDescs[i].firstTime().After(from)
	})
	// Find first chunk with start time after "through".
	throughIdx := sort.Search(len(s.chunkDescs), func(i int) bool {
		return s.chunkDescs[i].firstTime().After(through)
	})
	if fromIdx == len(s.chunkDescs) {
		// Even the last chunk starts before "from". Find out if the
		// series ends before "from" and we don't need to do anything.
		lt, err := s.chunkDescs[len(s.chunkDescs)-1].lastTime()
		if err != nil {
			return nil, err
		}
		if lt.Before(from) {
			return nil, nil
		}
	}
	if fromIdx > 0 {
		fromIdx--
	}
	if throughIdx == len(s.chunkDescs) {
		throughIdx--
	}
	var values []model.SamplePair
	in := metric.Interval{
		OldestInclusive: from,
		NewestInclusive: through,
	}
	for idx := fromIdx; idx <= throughIdx; idx++ {
		cd := s.chunkDescs[idx]
		chValues, err := rangeValues(cd.c.newIterator(), in)
		if err != nil {
			return nil, err
		}
		values = append(values, chValues...)
	}
	return values, nil
}

// Get all of the label values that are associated with a given label name.
func (i *Ingester) LabelValuesForLabelName(ctx context.Context, name model.LabelName) (model.LabelValues, error) {
	state, err := i.getStateFor(ctx)
	if err != nil {
		return nil, err
	}

	return state.index.lookupLabelValues(name), nil
}

func (i *Ingester) Stop() {
	i.stopLock.Lock()
	i.stopped = true
	i.stopLock.Unlock()

	close(i.quit)
	<-i.done
}

func (i *Ingester) loop() {
	defer func() {
		i.flushAllUsers(true)
		close(i.done)
		log.Infof("Ingester exited gracefully")
	}()

	tick := time.Tick(i.cfg.FlushCheckPeriod)
	for {
		select {
		case <-tick:
			i.flushAllUsers(false)
		case <-i.quit:
			return
		}
	}
}

func (i *Ingester) flushAllUsers(immediate bool) {
	log.Infof("Flushing chunks... (exiting: %v)", immediate)
	defer log.Infof("Done flushing chunks.")

	if i.chunkStore == nil {
		return
	}

	i.userStateLock.Lock()
	userIDs := make([]string, 0, len(i.userState))
	for userID := range i.userState {
		userIDs = append(userIDs, userID)
	}
	i.userStateLock.Unlock()

	var wg sync.WaitGroup
	for _, userID := range userIDs {
		wg.Add(1)
		go func() {
			i.flushUser(userID, immediate)
			wg.Done()
		}()
	}
	wg.Wait()
}

func (i *Ingester) flushUser(userID string, immediate bool) {
	log.Infof("Flushing user %s...", userID)
	defer log.Infof("Done flushing user %s.", userID)

	i.userStateLock.Lock()
	userState, ok := i.userState[userID]
	i.userStateLock.Unlock()

	// This should happen, right?
	if !ok {
		return
	}

	ctx := user.WithID(context.Background(), userID)
	i.flushAllSeries(ctx, userState, immediate)

	// TODO: this is probably slow, and could be done in a better way.
	i.userStateLock.Lock()
	if userState.fpToSeries.length() == 0 {
		delete(i.userState, userID)
	}
	i.userStateLock.Unlock()
}

func (i *Ingester) flushAllSeries(ctx context.Context, state *userState, immediate bool) {
	var wg sync.WaitGroup
	for pair := range state.fpToSeries.iter() {
		wg.Add(1)
		i.flushSeriesLimiter.Acquire()
		go func() {
			if err := i.flushSeries(ctx, state, pair.fp, pair.series, immediate); err != nil {
				log.Errorf("Failed to flush chunks for series: %v", err)
			}
			i.flushSeriesLimiter.Release()
			wg.Done()
		}()
	}
	wg.Wait()
}

func (i *Ingester) flushSeries(ctx context.Context, u *userState, fp model.Fingerprint, series *memorySeries, immediate bool) error {
	u.fpLocker.Lock(fp)

	// Decide what chunks to flush
	if immediate || time.Now().Sub(series.firstTime().Time()) > i.cfg.MaxChunkAge {
		series.headChunkClosed = true
		series.headChunkUsedByIterator = false
		series.head().maybePopulateLastTime()
	}
	chunks := series.chunkDescs
	if !series.headChunkClosed {
		chunks = chunks[:len(chunks)-1]
	}
	u.fpLocker.Unlock(fp)
	if len(chunks) == 0 {
		return nil
	}

	// flush the chunks without locking the series
	log.Infof("Flushing %d chunks", len(chunks))
	if err := i.flushChunks(ctx, fp, series.metric, chunks); err != nil {
		i.chunkStoreFailures.Add(float64(len(chunks)))
		return err
	}

	// now remove the chunks
	u.fpLocker.Lock(fp)
	series.chunkDescs = series.chunkDescs[len(chunks)-1:]
	i.memoryChunks.Sub(float64(len(chunks)))
	if len(series.chunkDescs) == 0 {
		u.fpToSeries.del(fp)
		u.index.delete(series.metric, fp)
	}
	u.fpLocker.Unlock(fp)
	return nil
}

func (i *Ingester) flushChunks(ctx context.Context, fp model.Fingerprint, metric model.Metric, chunks []*chunkDesc) error {
	wireChunks := make([]frank.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		buf := make([]byte, chunkLen)
		if err := chunk.c.marshalToBuf(buf); err != nil {
			return err
		}

		i.chunkUtilization.Observe(chunk.c.utilization())

		wireChunks = append(wireChunks, frank.Chunk{
			ID:      fmt.Sprintf("%d:%d:%d", fp, chunk.chunkFirstTime, chunk.chunkLastTime),
			From:    chunk.chunkFirstTime,
			Through: chunk.chunkLastTime,
			Metric:  metric,
			Data:    buf,
		})
	}
	return i.chunkStore.Put(ctx, wireChunks)
}

// Describe implements prometheus.Collector.
func (i *Ingester) Describe(ch chan<- *prometheus.Desc) {
	i.userStateLock.Lock()
	for _, state := range i.userState {
		state.mapper.Describe(ch)
	}
	i.userStateLock.Unlock()

	ch <- memorySeriesDesc
	ch <- memoryUsersDesc
	ch <- i.ingestedSamples.Desc()
	i.discardedSamples.Describe(ch)
	ch <- i.chunkUtilization.Desc()
	ch <- i.chunkStoreFailures.Desc()
	ch <- i.queries.Desc()
	ch <- i.queriedSamples.Desc()
}

// Collect implements prometheus.Collector.
func (i *Ingester) Collect(ch chan<- prometheus.Metric) {
	i.userStateLock.Lock()
	numUsers := len(i.userState)
	numSeries := 0
	for _, state := range i.userState {
		state.mapper.Collect(ch)
		numSeries += state.fpToSeries.length()
	}
	i.userStateLock.Unlock()

	ch <- prometheus.MustNewConstMetric(
		memorySeriesDesc,
		prometheus.GaugeValue,
		float64(numSeries),
	)
	ch <- prometheus.MustNewConstMetric(
		memoryUsersDesc,
		prometheus.GaugeValue,
		float64(numUsers),
	)
	ch <- i.ingestedSamples
	i.discardedSamples.Collect(ch)
	ch <- i.chunkUtilization
	ch <- i.chunkStoreFailures
	ch <- i.queries
	ch <- i.queriedSamples
}

type invertedIndex struct {
	mtx sync.RWMutex
	idx map[model.LabelName]map[model.LabelValue][]model.Fingerprint // entries are sorted in fp order?
}

func newInvertedIndex() *invertedIndex {
	return &invertedIndex{
		idx: map[model.LabelName]map[model.LabelValue][]model.Fingerprint{},
	}
}

func (i *invertedIndex) add(metric model.Metric, fp model.Fingerprint) {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	for name, value := range metric {
		values, ok := i.idx[name]
		if !ok {
			values = map[model.LabelValue][]model.Fingerprint{}
		}
		fingerprints := values[value]
		j := sort.Search(len(fingerprints), func(i int) bool {
			return fingerprints[i] >= fp
		})
		fingerprints = append(fingerprints, 0)
		copy(fingerprints[j+1:], fingerprints[j:])
		fingerprints[j] = fp
		values[value] = fingerprints
		i.idx[name] = values
	}
}

func (i *invertedIndex) lookup(matchers []*metric.LabelMatcher) []model.Fingerprint {
	if len(matchers) == 0 {
		return nil
	}
	i.mtx.RLock()
	defer i.mtx.RUnlock()

	// intersection is initially nil, which is a special case.
	var intersection []model.Fingerprint
	for _, matcher := range matchers {
		values, ok := i.idx[matcher.Name]
		if !ok {
			return nil
		}
		var toIntersect []model.Fingerprint
		for value, fps := range values {
			if matcher.Match(value) {
				toIntersect = merge(toIntersect, fps)
			}
		}
		intersection = intersect(intersection, toIntersect)
		if len(intersection) == 0 {
			return nil
		}
	}

	return intersection
}

func (i *invertedIndex) lookupLabelValues(name model.LabelName) model.LabelValues {
	i.mtx.RLock()
	defer i.mtx.RUnlock()

	values, ok := i.idx[name]
	if !ok {
		return nil
	}
	res := make(model.LabelValues, 0, len(values))
	for val := range values {
		res = append(res, val)
	}
	return res
}

func (i *invertedIndex) delete(metric model.Metric, fp model.Fingerprint) {
	i.mtx.Lock()
	defer i.mtx.Unlock()

	for name, value := range metric {
		values, ok := i.idx[name]
		if !ok {
			continue
		}
		fingerprints, ok := values[value]
		if !ok {
			continue
		}

		j := sort.Search(len(fingerprints), func(i int) bool {
			return fingerprints[i] >= fp
		})
		fingerprints = fingerprints[:j+copy(fingerprints[j:], fingerprints[j+1:])]

		if len(fingerprints) == 0 {
			delete(values, value)
		} else {
			values[value] = fingerprints
		}

		if len(values) == 0 {
			delete(i.idx, name)
		} else {
			i.idx[name] = values
		}
	}
}

// intersect two sorted lists of fingerprints.  Assumes there are no duplicate
// fingerprints within the input lists.
func intersect(a, b []model.Fingerprint) []model.Fingerprint {
	if a == nil {
		return b
	}
	result := []model.Fingerprint{}
	for i, j := 0, 0; i < len(a) && j < len(b); {
		if a[i] == b[j] {
			result = append(result, a[i])
		}
		if a[i] < b[j] {
			i++
		} else {
			j++
		}
	}
	return result
}

// merge two sorted lists of fingerprints.  Assumes there are no duplicate
// fingerprints between or within the input lists.
func merge(a, b []model.Fingerprint) []model.Fingerprint {
	result := make([]model.Fingerprint, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if a[i] < b[j] {
			result = append(result, a[i])
			i++
		} else {
			result = append(result, b[j])
			j++
		}
	}
	for ; i < len(a); i++ {
		result = append(result, a[i])
	}
	for ; j < len(b); j++ {
		result = append(result, b[j])
	}
	return result
}
