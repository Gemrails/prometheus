// Copyright 2013 Prometheus Team
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metric

import (
	"sort"
	"sync"
	"time"

	clientmodel "github.com/prometheus/client_golang/model"

	"github.com/prometheus/prometheus/model"
	"github.com/prometheus/prometheus/utility"
)

// Assuming sample rate of 1 / 15Hz, this allows for one hour's worth of
// storage per metric without any major reallocations.
const initialSeriesArenaSize = 4 * 60

// Models a given sample entry stored in the in-memory arena.
type value interface {
	// Gets the given value.
	get() model.SampleValue
}

// Models a single sample value.  It presumes that there is either no subsequent
// value seen or that any subsequent values are of a different value.
type singletonValue model.SampleValue

func (v singletonValue) get() model.SampleValue {
	return model.SampleValue(v)
}

type stream struct {
	sync.RWMutex

	metric clientmodel.Metric
	values model.Values
}

func (s *stream) add(timestamp time.Time, value model.SampleValue) {
	s.Lock()
	defer s.Unlock()

	// BUG(all): https://github.com/prometheus/prometheus/pull/265/files#r4336435.

	s.values = append(s.values, model.SamplePair{
		Timestamp: timestamp,
		Value:     value,
	})
}

func (s *stream) clone() model.Values {
	s.RLock()
	defer s.RUnlock()

	// BUG(all): Examine COW technique.

	clone := make(model.Values, len(s.values))
	copy(clone, s.values)

	return clone
}

func (s *stream) getValueAtTime(t time.Time) model.Values {
	s.RLock()
	defer s.RUnlock()

	// BUG(all): May be avenues for simplification.
	l := len(s.values)
	switch l {
	case 0:
		return model.Values{}
	case 1:
		return model.Values{s.values[0]}
	default:
		index := sort.Search(l, func(i int) bool {
			return !s.values[i].Timestamp.Before(t)
		})

		if index == 0 {
			return model.Values{s.values[0]}
		}
		if index == l {
			return model.Values{s.values[l-1]}
		}

		if s.values[index].Timestamp.Equal(t) {
			return model.Values{s.values[index]}
		}
		return model.Values{s.values[index-1], s.values[index]}
	}
}

func (s *stream) getBoundaryValues(in model.Interval) model.Values {
	s.RLock()
	defer s.RUnlock()

	oldest := sort.Search(len(s.values), func(i int) bool {
		return !s.values[i].Timestamp.Before(in.OldestInclusive)
	})

	newest := sort.Search(len(s.values), func(i int) bool {
		return s.values[i].Timestamp.After(in.NewestInclusive)
	})

	resultRange := s.values[oldest:newest]
	switch len(resultRange) {
	case 0:
		return model.Values{}
	case 1:
		return model.Values{resultRange[0]}
	default:
		return model.Values{resultRange[0], resultRange[len(resultRange)-1]}
	}
}

func (s *stream) getRangeValues(in model.Interval) model.Values {
	s.RLock()
	defer s.RUnlock()

	oldest := sort.Search(len(s.values), func(i int) bool {
		return !s.values[i].Timestamp.Before(in.OldestInclusive)
	})

	newest := sort.Search(len(s.values), func(i int) bool {
		return s.values[i].Timestamp.After(in.NewestInclusive)
	})

	result := make(model.Values, newest-oldest)
	copy(result, s.values[oldest:newest])

	return result
}

func newStream(metric clientmodel.Metric) *stream {
	return &stream{
		metric: metric,
		values: make(model.Values, 0, initialSeriesArenaSize),
	}
}

type memorySeriesStorage struct {
	sync.RWMutex

	wmCache                 *WatermarkCache
	fingerprintToSeries     map[clientmodel.Fingerprint]*stream
	labelPairToFingerprints map[model.LabelPair]model.Fingerprints
	labelNameToFingerprints map[model.LabelName]model.Fingerprints
}

type MemorySeriesOptions struct {
	// If provided, this WatermarkCache will be updated for any samples that are
	// appended to the memorySeriesStorage.
	WatermarkCache *WatermarkCache
}

func (s *memorySeriesStorage) AppendSamples(samples clientmodel.Samples) error {
	for _, sample := range samples {
		s.AppendSample(sample)
	}

	return nil
}

func (s *memorySeriesStorage) AppendSample(sample clientmodel.Sample) error {
	s.Lock()
	defer s.Unlock()

	metric := sample.Metric
	fingerprint := model.NewFingerprintFromMetric(metric)
	series, ok := s.fingerprintToSeries[*fingerprint]

	if s.wmCache != nil {
		s.wmCache.Set(fingerprint, &Watermarks{High: sample.Timestamp})
	}

	if !ok {
		series = newStream(metric)
		s.fingerprintToSeries[*fingerprint] = series

		for k, v := range metric {
			labelPair := model.LabelPair{
				Name:  k,
				Value: v,
			}
			labelPairValues := s.labelPairToFingerprints[labelPair]
			labelPairValues = append(labelPairValues, fingerprint)
			s.labelPairToFingerprints[labelPair] = labelPairValues

			labelNameValues := s.labelNameToFingerprints[k]
			labelNameValues = append(labelNameValues, fingerprint)
			s.labelNameToFingerprints[k] = labelNameValues
		}
	}

	series.add(sample.Timestamp, sample.Value)

	return nil
}

// Append raw samples, bypassing indexing. Only used to add data to views,
// which don't need to lookup by metric.
func (s *memorySeriesStorage) appendSamplesWithoutIndexing(fingerprint *clientmodel.Fingerprint, samples model.Values) {
	s.Lock()
	defer s.Unlock()

	series, ok := s.fingerprintToSeries[*fingerprint]

	if !ok {
		series = newStream(clientmodel.Metric{})
		s.fingerprintToSeries[*fingerprint] = series
	}

	for _, sample := range samples {
		series.add(sample.Timestamp, sample.Value)
	}
}

func (s *memorySeriesStorage) GetFingerprintsForLabelSet(l model.LabelSet) (fingerprints model.Fingerprints, err error) {
	s.RLock()
	defer s.RUnlock()

	sets := []utility.Set{}
	for k, v := range l {
		values := s.labelPairToFingerprints[model.LabelPair{
			Name:  k,
			Value: v,
		}]
		set := utility.Set{}
		for _, fingerprint := range values {
			set.Add(*fingerprint)
		}
		sets = append(sets, set)
	}

	setCount := len(sets)
	if setCount == 0 {
		return fingerprints, nil
	}

	base := sets[0]
	for i := 1; i < setCount; i++ {
		base = base.Intersection(sets[i])
	}
	for _, e := range base.Elements() {
		fingerprint := e.(clientmodel.Fingerprint)
		fingerprints = append(fingerprints, &fingerprint)
	}

	return fingerprints, nil
}

func (s *memorySeriesStorage) GetFingerprintsForLabelName(l model.LabelName) (model.Fingerprints, error) {
	s.RLock()
	defer s.RUnlock()

	values, ok := s.labelNameToFingerprints[l]
	if !ok {
		return nil, nil
	}

	fingerprints := make(model.Fingerprints, len(values))
	copy(fingerprints, values)

	return fingerprints, nil
}

func (s *memorySeriesStorage) GetMetricForFingerprint(f *clientmodel.Fingerprint) (clientmodel.Metric, error) {
	s.RLock()
	defer s.RUnlock()

	series, ok := s.fingerprintToSeries[*f]
	if !ok {
		return nil, nil
	}

	metric := clientmodel.Metric{}
	for label, value := range series.metric {
		metric[label] = value
	}

	return metric, nil
}

func (s *memorySeriesStorage) CloneSamples(f *clientmodel.Fingerprint) model.Values {
	s.RLock()
	defer s.RUnlock()

	series, ok := s.fingerprintToSeries[*f]
	if !ok {
		return nil
	}

	return series.clone()
}

func (s *memorySeriesStorage) GetValueAtTime(f *clientmodel.Fingerprint, t time.Time) model.Values {
	s.RLock()
	defer s.RUnlock()

	series, ok := s.fingerprintToSeries[*f]
	if !ok {
		return nil
	}

	return series.getValueAtTime(t)
}

func (s *memorySeriesStorage) GetBoundaryValues(f *clientmodel.Fingerprint, i model.Interval) model.Values {
	s.RLock()
	defer s.RUnlock()

	series, ok := s.fingerprintToSeries[*f]
	if !ok {
		return nil
	}

	return series.getBoundaryValues(i)
}

func (s *memorySeriesStorage) GetRangeValues(f *clientmodel.Fingerprint, i model.Interval) model.Values {
	s.RLock()
	defer s.RUnlock()

	series, ok := s.fingerprintToSeries[*f]

	if !ok {
		return nil
	}

	return series.getRangeValues(i)
}

func (s *memorySeriesStorage) Close() {
	s.Lock()
	defer s.Unlock()

	s.fingerprintToSeries = map[clientmodel.Fingerprint]*stream{}
	s.labelPairToFingerprints = map[model.LabelPair]model.Fingerprints{}
	s.labelNameToFingerprints = map[model.LabelName]model.Fingerprints{}
}

func (s *memorySeriesStorage) GetAllValuesForLabel(labelName model.LabelName) (values model.LabelValues, err error) {
	s.RLock()
	defer s.RUnlock()

	valueSet := map[model.LabelValue]bool{}
	for _, series := range s.fingerprintToSeries {
		if value, ok := series.metric[labelName]; ok {
			if !valueSet[value] {
				values = append(values, value)
				valueSet[value] = true
			}
		}
	}

	return
}

func NewMemorySeriesStorage(o MemorySeriesOptions) *memorySeriesStorage {
	return &memorySeriesStorage{
		fingerprintToSeries:     make(map[clientmodel.Fingerprint]*stream),
		labelPairToFingerprints: make(map[model.LabelPair]model.Fingerprints),
		labelNameToFingerprints: make(map[model.LabelName]model.Fingerprints),
		wmCache:                 o.WatermarkCache,
	}
}
