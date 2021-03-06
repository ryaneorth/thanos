// Copyright (c) The Thanos Authors.
// Licensed under the Apache License 2.0.

package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fortytw2/leaktest"
	"github.com/go-kit/kit/log"
	"github.com/leanovate/gopter"
	"github.com/leanovate/gopter/gen"
	"github.com/leanovate/gopter/prop"
	"github.com/oklog/ulid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/pkg/relabel"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/encoding"
	"github.com/prometheus/prometheus/tsdb/index"
	"github.com/thanos-io/thanos/pkg/block"
	"github.com/thanos-io/thanos/pkg/block/indexheader"
	"github.com/thanos-io/thanos/pkg/block/metadata"
	"github.com/thanos-io/thanos/pkg/compact/downsample"
	"github.com/thanos-io/thanos/pkg/objstore"
	"github.com/thanos-io/thanos/pkg/objstore/filesystem"
	"github.com/thanos-io/thanos/pkg/objstore/inmem"
	"github.com/thanos-io/thanos/pkg/pool"
	storecache "github.com/thanos-io/thanos/pkg/store/cache"
	"github.com/thanos-io/thanos/pkg/store/storepb"
	"github.com/thanos-io/thanos/pkg/testutil"
	"github.com/thanos-io/thanos/pkg/testutil/e2eutil"
	"gopkg.in/yaml.v2"
)

var emptyRelabelConfig = make([]*relabel.Config, 0)

func TestBucketBlock_Property(t *testing.T) {
	parameters := gopter.DefaultTestParameters()
	parameters.Rng.Seed(2000)
	parameters.MinSuccessfulTests = 20000
	properties := gopter.NewProperties(parameters)

	set := newBucketBlockSet(labels.Labels{})

	type resBlock struct {
		mint, maxt int64
		window     int64
	}
	// This input resembles a typical production-level block layout
	// in remote object storage.
	input := []resBlock{
		{window: downsample.ResLevel0, mint: 0, maxt: 100},
		{window: downsample.ResLevel0, mint: 100, maxt: 200},
		// Compaction level 2 begins but not downsampling (8 hour block length).
		{window: downsample.ResLevel0, mint: 200, maxt: 600},
		{window: downsample.ResLevel0, mint: 600, maxt: 1000},
		// Compaction level 3 begins, Some of it is downsampled but still retained (48 hour block length).
		{window: downsample.ResLevel0, mint: 1000, maxt: 1750},
		{window: downsample.ResLevel1, mint: 1000, maxt: 1750},
		// Compaction level 4 begins, different downsampling levels cover the same (336 hour block length).
		{window: downsample.ResLevel0, mint: 1750, maxt: 7000},
		{window: downsample.ResLevel1, mint: 1750, maxt: 7000},
		{window: downsample.ResLevel2, mint: 1750, maxt: 7000},
		// Compaction level 4 already happened, raw samples have been deleted.
		{window: downsample.ResLevel0, mint: 7000, maxt: 14000},
		{window: downsample.ResLevel1, mint: 7000, maxt: 14000},
		// Compaction level 4 already happened, raw and downsample res level 1 samples have been deleted.
		{window: downsample.ResLevel2, mint: 14000, maxt: 21000},
	}

	for _, in := range input {
		var m metadata.Meta
		m.Thanos.Downsample.Resolution = in.window
		m.MinTime = in.mint
		m.MaxTime = in.maxt

		testutil.Ok(t, set.add(&bucketBlock{meta: &m}))
	}

	properties.Property("getFor always gets at least some data in range", prop.ForAllNoShrink(
		func(low, high, maxResolution int64) bool {
			// Bogus case.
			if low >= high {
				return true
			}

			res := set.getFor(low, high, maxResolution)

			// The data that we get must all encompass our requested range.
			if len(res) == 1 && (res[0].meta.Thanos.Downsample.Resolution > maxResolution ||
				res[0].meta.MinTime > low) {
				return false
			} else if len(res) > 1 {
				mint := int64(21001)
				for i := 0; i < len(res)-1; i++ {
					if res[i].meta.Thanos.Downsample.Resolution > maxResolution {
						return false
					}
					if res[i+1].meta.MinTime != res[i].meta.MaxTime {
						return false
					}
					if res[i].meta.MinTime < mint {
						mint = res[i].meta.MinTime
					}
				}
				if res[len(res)-1].meta.MinTime < mint {
					mint = res[len(res)-1].meta.MinTime
				}
				if low < mint {
					return false
				}

			}
			return true
		}, gen.Int64Range(0, 21000), gen.Int64Range(0, 21000), gen.Int64Range(0, 60*60*1000)),
	)

	properties.Property("getFor always gets all data in range", prop.ForAllNoShrink(
		func(low, high int64) bool {
			// Bogus case.
			if low >= high {
				return true
			}

			maxResolution := downsample.ResLevel2
			res := set.getFor(low, high, maxResolution)

			// The data that we get must all encompass our requested range.
			if len(res) == 1 && (res[0].meta.Thanos.Downsample.Resolution > maxResolution ||
				res[0].meta.MinTime > low || res[0].meta.MaxTime < high) {
				return false
			} else if len(res) > 1 {
				mint := int64(21001)
				maxt := int64(0)
				for i := 0; i < len(res)-1; i++ {
					if res[i+1].meta.MinTime != res[i].meta.MaxTime {
						return false
					}
					if res[i].meta.MinTime < mint {
						mint = res[i].meta.MinTime
					}
					if res[i].meta.MaxTime > maxt {
						maxt = res[i].meta.MaxTime
					}
				}
				if res[len(res)-1].meta.MinTime < mint {
					mint = res[len(res)-1].meta.MinTime
				}
				if res[len(res)-1].meta.MaxTime > maxt {
					maxt = res[len(res)-1].meta.MaxTime
				}
				if low < mint {
					return false
				}
				if high > maxt {
					return false
				}

			}
			return true
		}, gen.Int64Range(0, 21000), gen.Int64Range(0, 21000)),
	)

	properties.TestingRun(t)
}

func TestBucketBlockSet_addGet(t *testing.T) {
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	set := newBucketBlockSet(labels.Labels{})

	type resBlock struct {
		mint, maxt int64
		window     int64
	}
	// Input is expected to be sorted. It is sorted in addBlock.
	input := []resBlock{
		// Blocks from 0 to 100 with raw resolution.
		{window: downsample.ResLevel0, mint: 0, maxt: 100},
		{window: downsample.ResLevel0, mint: 100, maxt: 200},
		{window: downsample.ResLevel0, mint: 100, maxt: 200}, // Same overlap.
		{window: downsample.ResLevel0, mint: 200, maxt: 299}, // Short overlap.
		{window: downsample.ResLevel0, mint: 200, maxt: 300},
		{window: downsample.ResLevel0, mint: 300, maxt: 400},
		{window: downsample.ResLevel0, mint: 300, maxt: 600}, // Long overlap.
		{window: downsample.ResLevel0, mint: 400, maxt: 500},
		// Lower resolution data not covering last block.
		{window: downsample.ResLevel1, mint: 0, maxt: 100},
		{window: downsample.ResLevel1, mint: 100, maxt: 200},
		{window: downsample.ResLevel1, mint: 200, maxt: 300},
		{window: downsample.ResLevel1, mint: 300, maxt: 400},
		// Lower resolution data only covering middle blocks.
		{window: downsample.ResLevel2, mint: 100, maxt: 200},
		{window: downsample.ResLevel2, mint: 200, maxt: 300},
	}

	for _, in := range input {
		var m metadata.Meta
		m.Thanos.Downsample.Resolution = in.window
		m.MinTime = in.mint
		m.MaxTime = in.maxt

		testutil.Ok(t, set.add(&bucketBlock{meta: &m}))
	}

	for _, c := range []struct {
		mint, maxt    int64
		maxResolution int64
		res           []resBlock
	}{
		{
			mint:          -100,
			maxt:          1000,
			maxResolution: 0,
			res: []resBlock{
				{window: downsample.ResLevel0, mint: 0, maxt: 100},
				{window: downsample.ResLevel0, mint: 100, maxt: 200},
				{window: downsample.ResLevel0, mint: 100, maxt: 200},
				{window: downsample.ResLevel0, mint: 200, maxt: 299},
				{window: downsample.ResLevel0, mint: 200, maxt: 300},
				{window: downsample.ResLevel0, mint: 300, maxt: 400},
				{window: downsample.ResLevel0, mint: 300, maxt: 600},
				{window: downsample.ResLevel0, mint: 400, maxt: 500},
			},
		}, {
			mint:          100,
			maxt:          400,
			maxResolution: downsample.ResLevel1 - 1,
			res: []resBlock{
				{window: downsample.ResLevel0, mint: 100, maxt: 200},
				{window: downsample.ResLevel0, mint: 100, maxt: 200},
				{window: downsample.ResLevel0, mint: 200, maxt: 299},
				{window: downsample.ResLevel0, mint: 200, maxt: 300},
				{window: downsample.ResLevel0, mint: 300, maxt: 400},
				{window: downsample.ResLevel0, mint: 300, maxt: 600},
				// Block intervals are half-open: [b.MinTime, b.MaxTime), so 400-500 contains single sample.
				{window: downsample.ResLevel0, mint: 400, maxt: 500},
			},
		}, {
			mint:          100,
			maxt:          500,
			maxResolution: downsample.ResLevel1,
			res: []resBlock{
				{window: downsample.ResLevel1, mint: 100, maxt: 200},
				{window: downsample.ResLevel1, mint: 200, maxt: 300},
				{window: downsample.ResLevel1, mint: 300, maxt: 400},
				{window: downsample.ResLevel0, mint: 300, maxt: 600},
				{window: downsample.ResLevel0, mint: 400, maxt: 500},
			},
		}, {
			mint:          0,
			maxt:          500,
			maxResolution: downsample.ResLevel2,
			res: []resBlock{
				{window: downsample.ResLevel1, mint: 0, maxt: 100},
				{window: downsample.ResLevel2, mint: 100, maxt: 200},
				{window: downsample.ResLevel2, mint: 200, maxt: 300},
				{window: downsample.ResLevel1, mint: 300, maxt: 400},
				{window: downsample.ResLevel0, mint: 300, maxt: 600},
				{window: downsample.ResLevel0, mint: 400, maxt: 500},
			},
		},
	} {
		t.Run("", func(t *testing.T) {
			var exp []*bucketBlock
			for _, b := range c.res {
				var m metadata.Meta
				m.Thanos.Downsample.Resolution = b.window
				m.MinTime = b.mint
				m.MaxTime = b.maxt
				exp = append(exp, &bucketBlock{meta: &m})
			}
			testutil.Equals(t, exp, set.getFor(c.mint, c.maxt, c.maxResolution))
		})
	}
}

func TestBucketBlockSet_remove(t *testing.T) {
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	set := newBucketBlockSet(labels.Labels{})

	type resBlock struct {
		id         ulid.ULID
		mint, maxt int64
	}
	input := []resBlock{
		{id: ulid.MustNew(1, nil), mint: 0, maxt: 100},
		{id: ulid.MustNew(2, nil), mint: 100, maxt: 200},
		{id: ulid.MustNew(3, nil), mint: 200, maxt: 300},
	}

	for _, in := range input {
		var m metadata.Meta
		m.ULID = in.id
		m.MinTime = in.mint
		m.MaxTime = in.maxt
		testutil.Ok(t, set.add(&bucketBlock{meta: &m}))
	}
	set.remove(input[1].id)
	res := set.getFor(0, 300, 0)

	testutil.Equals(t, 2, len(res))
	testutil.Equals(t, input[0].id, res[0].meta.ULID)
	testutil.Equals(t, input[2].id, res[1].meta.ULID)
}

func TestBucketBlockSet_labelMatchers(t *testing.T) {
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	set := newBucketBlockSet(labels.FromStrings("a", "b", "c", "d"))

	cases := []struct {
		in    []*labels.Matcher
		res   []*labels.Matcher
		match bool
	}{
		{
			in:    []*labels.Matcher{},
			res:   []*labels.Matcher{},
			match: true,
		},
		{
			in: []*labels.Matcher{
				{Type: labels.MatchEqual, Name: "a", Value: "b"},
				{Type: labels.MatchEqual, Name: "c", Value: "d"},
			},
			res:   []*labels.Matcher{},
			match: true,
		},
		{
			in: []*labels.Matcher{
				{Type: labels.MatchEqual, Name: "a", Value: "b"},
				{Type: labels.MatchEqual, Name: "c", Value: "b"},
			},
			match: false,
		},
		{
			in: []*labels.Matcher{
				{Type: labels.MatchEqual, Name: "a", Value: "b"},
				{Type: labels.MatchEqual, Name: "e", Value: "f"},
			},
			res: []*labels.Matcher{
				{Type: labels.MatchEqual, Name: "e", Value: "f"},
			},
			match: true,
		},
		// Those are matchers mentioned here: https://github.com/prometheus/prometheus/pull/3578#issuecomment-351653555
		// We want to provide explicit tests that says when Thanos supports its and when not. We don't support it here in
		// external labelset level.
		{
			in: []*labels.Matcher{
				{Type: labels.MatchNotEqual, Name: "", Value: "x"},
			},
			res: []*labels.Matcher{
				{Type: labels.MatchNotEqual, Name: "", Value: "x"},
			},
			match: true,
		},
		{
			in: []*labels.Matcher{
				{Type: labels.MatchNotEqual, Name: "", Value: "d"},
			},
			res: []*labels.Matcher{
				{Type: labels.MatchNotEqual, Name: "", Value: "d"},
			},
			match: true,
		},
	}
	for _, c := range cases {
		res, ok := set.labelMatchers(c.in...)
		testutil.Equals(t, c.match, ok)
		testutil.Equals(t, c.res, res)
	}
}

func TestGapBasedPartitioner_Partition(t *testing.T) {
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	const maxGapSize = 1024 * 512

	for _, c := range []struct {
		input    [][2]int
		expected []part
	}{
		{
			input:    [][2]int{{1, 10}},
			expected: []part{{start: 1, end: 10, elemRng: [2]int{0, 1}}},
		},
		{
			input:    [][2]int{{1, 2}, {3, 5}, {7, 10}},
			expected: []part{{start: 1, end: 10, elemRng: [2]int{0, 3}}},
		},
		{
			input: [][2]int{
				{1, 2},
				{3, 5},
				{20, 30},
				{maxGapSize + 31, maxGapSize + 32},
			},
			expected: []part{
				{start: 1, end: 30, elemRng: [2]int{0, 3}},
				{start: maxGapSize + 31, end: maxGapSize + 32, elemRng: [2]int{3, 4}},
			},
		},
		// Overlapping ranges.
		{
			input: [][2]int{
				{1, 30},
				{1, 4},
				{3, 28},
				{maxGapSize + 31, maxGapSize + 32},
				{maxGapSize + 31, maxGapSize + 40},
			},
			expected: []part{
				{start: 1, end: 30, elemRng: [2]int{0, 3}},
				{start: maxGapSize + 31, end: maxGapSize + 40, elemRng: [2]int{3, 5}},
			},
		},
		{
			input: [][2]int{
				// Mimick AllPostingsKey, where range specified whole range.
				{1, 15},
				{1, maxGapSize + 100},
				{maxGapSize + 31, maxGapSize + 40},
			},
			expected: []part{{start: 1, end: maxGapSize + 100, elemRng: [2]int{0, 3}}},
		},
	} {
		res := gapBasedPartitioner{maxGapSize: maxGapSize}.Partition(len(c.input), func(i int) (uint64, uint64) {
			return uint64(c.input[i][0]), uint64(c.input[i][1])
		})
		testutil.Equals(t, c.expected, res)
	}
}

func TestBucketStore_Info(t *testing.T) {
	defer leaktest.CheckTimeout(t, 10*time.Second)()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir, err := ioutil.TempDir("", "bucketstore-test")
	testutil.Ok(t, err)

	defer testutil.Ok(t, os.RemoveAll(dir))

	bucketStore, err := NewBucketStore(
		nil,
		nil,
		nil,
		nil,
		dir,
		noopCache{},
		2e5,
		0,
		0,
		false,
		20,
		allowAllFilterConf,
		true,
		true,
		true,
	)
	testutil.Ok(t, err)

	resp, err := bucketStore.Info(ctx, &storepb.InfoRequest{})
	testutil.Ok(t, err)

	testutil.Equals(t, storepb.StoreType_STORE, resp.StoreType)
	testutil.Equals(t, int64(math.MaxInt64), resp.MinTime)
	testutil.Equals(t, int64(math.MinInt64), resp.MaxTime)
	testutil.Equals(t, []storepb.LabelSet(nil), resp.LabelSets)
	testutil.Equals(t, []storepb.Label(nil), resp.Labels)
}

type recorder struct {
	mtx sync.Mutex
	objstore.Bucket

	getRangeTouched []string
	getTouched      []string
}

func (r *recorder) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	r.getTouched = append(r.getTouched, name)
	return r.Bucket.Get(ctx, name)
}

func (r *recorder) GetRange(ctx context.Context, name string, off, length int64) (io.ReadCloser, error) {
	r.mtx.Lock()
	defer r.mtx.Unlock()

	r.getRangeTouched = append(r.getRangeTouched, name)
	return r.Bucket.GetRange(ctx, name, off, length)
}

func TestBucketStore_Sharding(t *testing.T) {
	ctx := context.Background()
	logger := log.NewNopLogger()

	dir, err := ioutil.TempDir("", "test-sharding-prepare")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(dir)) }()

	bkt := inmem.NewBucket()
	series := []labels.Labels{labels.FromStrings("a", "1", "b", "1")}

	id1, err := e2eutil.CreateBlock(ctx, dir, series, 10, 0, 1000, labels.Labels{{Name: "cluster", Value: "a"}, {Name: "region", Value: "r1"}}, 0)
	testutil.Ok(t, err)
	testutil.Ok(t, block.Upload(ctx, logger, bkt, filepath.Join(dir, id1.String())))

	id2, err := e2eutil.CreateBlock(ctx, dir, series, 10, 1000, 2000, labels.Labels{{Name: "cluster", Value: "a"}, {Name: "region", Value: "r1"}}, 0)
	testutil.Ok(t, err)
	testutil.Ok(t, block.Upload(ctx, logger, bkt, filepath.Join(dir, id2.String())))

	id3, err := e2eutil.CreateBlock(ctx, dir, series, 10, 0, 1000, labels.Labels{{Name: "cluster", Value: "b"}, {Name: "region", Value: "r1"}}, 0)
	testutil.Ok(t, err)
	testutil.Ok(t, block.Upload(ctx, logger, bkt, filepath.Join(dir, id3.String())))

	id4, err := e2eutil.CreateBlock(ctx, dir, series, 10, 0, 1000, labels.Labels{{Name: "cluster", Value: "a"}, {Name: "region", Value: "r2"}}, 0)
	testutil.Ok(t, err)
	testutil.Ok(t, block.Upload(ctx, logger, bkt, filepath.Join(dir, id4.String())))

	if ok := t.Run("new_runs", func(t *testing.T) {
		testSharding(t, "", bkt, id1, id2, id3, id4)
	}); !ok {
		return
	}

	dir2, err := ioutil.TempDir("", "test-sharding2")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(dir2)) }()

	if ok := t.Run("reuse_disk", func(t *testing.T) {
		testSharding(t, dir2, bkt, id1, id2, id3, id4)
	}); !ok {
		return
	}

}

func testSharding(t *testing.T, reuseDisk string, bkt objstore.Bucket, all ...ulid.ULID) {
	var cached []ulid.ULID

	logger := log.NewLogfmtLogger(os.Stderr)
	for _, sc := range []struct {
		name              string
		relabel           string
		expectedIDs       []ulid.ULID
		expectedAdvLabels []storepb.LabelSet
	}{
		{
			name:        "no sharding",
			expectedIDs: all,
			expectedAdvLabels: []storepb.LabelSet{
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "a"},
						{Name: "region", Value: "r1"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "a"},
						{Name: "region", Value: "r2"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "b"},
						{Name: "region", Value: "r1"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: CompatibilityTypeLabelName, Value: "store"},
					},
				},
			},
		},
		{
			name: "drop cluster=a sources",
			relabel: `
            - action: drop
              regex: "a"
              source_labels:
              - cluster
            `,
			expectedIDs: []ulid.ULID{all[2]},
			expectedAdvLabels: []storepb.LabelSet{
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "b"},
						{Name: "region", Value: "r1"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: CompatibilityTypeLabelName, Value: "store"},
					},
				},
			},
		},
		{
			name: "keep only cluster=a sources",
			relabel: `
            - action: keep
              regex: "a"
              source_labels:
              - cluster
            `,
			expectedIDs: []ulid.ULID{all[0], all[1], all[3]},
			expectedAdvLabels: []storepb.LabelSet{
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "a"},
						{Name: "region", Value: "r1"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "a"},
						{Name: "region", Value: "r2"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: CompatibilityTypeLabelName, Value: "store"},
					},
				},
			},
		},
		{
			name: "keep only cluster=a without .*2 region sources",
			relabel: `
            - action: keep
              regex: "a"
              source_labels:
              - cluster
            - action: drop
              regex: ".*2"
              source_labels:
              - region
            `,
			expectedIDs: []ulid.ULID{all[0], all[1]},
			expectedAdvLabels: []storepb.LabelSet{
				{
					Labels: []storepb.Label{
						{Name: "cluster", Value: "a"},
						{Name: "region", Value: "r1"},
					},
				},
				{
					Labels: []storepb.Label{
						{Name: CompatibilityTypeLabelName, Value: "store"},
					},
				},
			},
		},
		{
			name: "drop all",
			relabel: `
            - action: drop
              regex: "a"
              source_labels:
              - cluster
            - action: drop
              regex: "r1"
              source_labels:
              - region
            `,
			expectedIDs:       []ulid.ULID{},
			expectedAdvLabels: []storepb.LabelSet(nil),
		},
	} {
		t.Run(sc.name, func(t *testing.T) {
			dir := reuseDisk

			if dir == "" {
				var err error
				dir, err = ioutil.TempDir("", "test-sharding")
				testutil.Ok(t, err)
				defer func() { testutil.Ok(t, os.RemoveAll(dir)) }()
			}

			var relabelConf []*relabel.Config
			testutil.Ok(t, yaml.Unmarshal([]byte(sc.relabel), &relabelConf))

			rec := &recorder{Bucket: bkt}
			metaFetcher, err := block.NewMetaFetcher(logger, 20, bkt, dir, nil, []block.MetadataFilter{
				block.NewTimePartitionMetaFilter(allowAllFilterConf.MinTime, allowAllFilterConf.MaxTime),
				block.NewLabelShardedMetaFilter(relabelConf),
			})
			testutil.Ok(t, err)

			bucketStore, err := NewBucketStore(
				logger,
				nil,
				rec,
				metaFetcher,
				dir,
				noopCache{},
				0,
				0,
				99,
				false,
				20,
				allowAllFilterConf,
				true,
				true,
				true,
			)
			testutil.Ok(t, err)

			testutil.Ok(t, bucketStore.InitialSync(context.Background()))

			// Check "stored" blocks.
			ids := make([]ulid.ULID, 0, len(bucketStore.blocks))
			for id := range bucketStore.blocks {
				ids = append(ids, id)
			}
			sort.Slice(ids, func(i, j int) bool {
				return ids[i].Compare(ids[j]) < 0
			})
			testutil.Equals(t, sc.expectedIDs, ids)

			// Check Info endpoint.
			resp, err := bucketStore.Info(context.Background(), &storepb.InfoRequest{})
			testutil.Ok(t, err)

			testutil.Equals(t, storepb.StoreType_STORE, resp.StoreType)
			testutil.Equals(t, []storepb.Label(nil), resp.Labels)
			testutil.Equals(t, sc.expectedAdvLabels, resp.LabelSets)

			// Make sure we don't download files we did not expect to.
			// Regression test: https://github.com/thanos-io/thanos/issues/1664

			// Sort records. We load blocks concurrently so operations might be not ordered.
			sort.Strings(rec.getRangeTouched)

			// With binary header nothing should be downloaded fully.
			testutil.Equals(t, []string(nil), rec.getTouched)
			if reuseDisk != "" {
				testutil.Equals(t, expectedTouchedBlockOps(all, sc.expectedIDs, cached), rec.getRangeTouched)
				cached = sc.expectedIDs
				return
			}

			testutil.Equals(t, expectedTouchedBlockOps(all, sc.expectedIDs, nil), rec.getRangeTouched)
		})
	}
}

func expectedTouchedBlockOps(all []ulid.ULID, expected []ulid.ULID, cached []ulid.ULID) []string {
	var ops []string
	for _, id := range all {
		blockCached := false
		for _, fid := range cached {
			if id.Compare(fid) == 0 {
				blockCached = true
				break
			}
		}
		if blockCached {
			continue
		}

		found := false
		for _, fid := range expected {
			if id.Compare(fid) == 0 {
				found = true
				break
			}
		}

		if found {
			ops = append(ops,
				// To create binary header we touch part of index few times.
				path.Join(id.String(), block.IndexFilename), // Version.
				path.Join(id.String(), block.IndexFilename), // TOC.
				path.Join(id.String(), block.IndexFilename), // Symbols.
				path.Join(id.String(), block.IndexFilename), // PostingOffsets.
			)
		}
	}
	sort.Strings(ops)
	return ops
}

// Regression tests against: https://github.com/thanos-io/thanos/issues/1983.
func TestReadIndexCache_LoadSeries(t *testing.T) {
	bkt := inmem.NewBucket()

	s := newBucketStoreMetrics(nil)
	b := &bucketBlock{
		meta: &metadata.Meta{
			BlockMeta: tsdb.BlockMeta{
				ULID: ulid.MustNew(1, nil),
			},
		},
		bkt:             bkt,
		seriesRefetches: s.seriesRefetches,
		logger:          log.NewNopLogger(),
		indexCache:      noopCache{},
	}

	buf := encoding.Encbuf{}
	buf.PutByte(0)
	buf.PutByte(0)
	buf.PutUvarint(10)
	buf.PutString("aaaaaaaaaa")
	buf.PutUvarint(10)
	buf.PutString("bbbbbbbbbb")
	buf.PutUvarint(10)
	buf.PutString("cccccccccc")
	testutil.Ok(t, bkt.Upload(context.Background(), filepath.Join(b.meta.ULID.String(), block.IndexFilename), bytes.NewReader(buf.Get())))

	r := bucketIndexReader{
		block:        b,
		stats:        &queryStats{},
		loadedSeries: map[uint64][]byte{},
	}

	// Success with no refetches.
	testutil.Ok(t, r.loadSeries(context.TODO(), []uint64{2, 13, 24}, false, 2, 100))
	testutil.Equals(t, map[uint64][]byte{
		2:  []byte("aaaaaaaaaa"),
		13: []byte("bbbbbbbbbb"),
		24: []byte("cccccccccc"),
	}, r.loadedSeries)
	testutil.Equals(t, float64(0), promtest.ToFloat64(s.seriesRefetches))

	// Success with 2 refetches.
	r.loadedSeries = map[uint64][]byte{}
	testutil.Ok(t, r.loadSeries(context.TODO(), []uint64{2, 13, 24}, false, 2, 15))
	testutil.Equals(t, map[uint64][]byte{
		2:  []byte("aaaaaaaaaa"),
		13: []byte("bbbbbbbbbb"),
		24: []byte("cccccccccc"),
	}, r.loadedSeries)
	testutil.Equals(t, float64(2), promtest.ToFloat64(s.seriesRefetches))

	// Success with refetch on first element.
	r.loadedSeries = map[uint64][]byte{}
	testutil.Ok(t, r.loadSeries(context.TODO(), []uint64{2}, false, 2, 5))
	testutil.Equals(t, map[uint64][]byte{
		2: []byte("aaaaaaaaaa"),
	}, r.loadedSeries)
	testutil.Equals(t, float64(3), promtest.ToFloat64(s.seriesRefetches))

	buf.Reset()
	buf.PutByte(0)
	buf.PutByte(0)
	buf.PutUvarint(10)
	buf.PutString("aaaaaaa")
	testutil.Ok(t, bkt.Upload(context.Background(), filepath.Join(b.meta.ULID.String(), block.IndexFilename), bytes.NewReader(buf.Get())))

	// Fail, but no recursion at least.
	testutil.NotOk(t, r.loadSeries(context.TODO(), []uint64{2, 13, 24}, false, 1, 15))
}

func TestBucketIndexReader_ExpandedPostings(t *testing.T) {
	tb := testutil.NewTB(t)

	tmpDir, err := ioutil.TempDir("", "test-expanded-postings")
	testutil.Ok(tb, err)
	defer func() { testutil.Ok(tb, os.RemoveAll(tmpDir)) }()

	bkt, err := filesystem.NewBucket(filepath.Join(tmpDir, "bkt"))
	testutil.Ok(tb, err)
	defer func() { testutil.Ok(tb, bkt.Close()) }()

	id := uploadTestBlock(tb, tmpDir, bkt, 500)

	r, err := indexheader.NewBinaryReader(context.Background(), log.NewNopLogger(), bkt, tmpDir, id)
	testutil.Ok(tb, err)

	benchmarkExpandedPostings(tb, bkt, id, r, 500)
}

func BenchmarkBucketIndexReader_ExpandedPostings(b *testing.B) {
	tb := testutil.NewTB(b)

	tmpDir, err := ioutil.TempDir("", "bench-expanded-postings")
	testutil.Ok(tb, err)
	defer func() { testutil.Ok(tb, os.RemoveAll(tmpDir)) }()

	bkt, err := filesystem.NewBucket(filepath.Join(tmpDir, "bkt"))
	testutil.Ok(tb, err)
	defer func() { testutil.Ok(tb, bkt.Close()) }()

	id := uploadTestBlock(tb, tmpDir, bkt, 50e5)
	r, err := indexheader.NewBinaryReader(context.Background(), log.NewNopLogger(), bkt, tmpDir, id)
	testutil.Ok(tb, err)

	benchmarkExpandedPostings(tb, bkt, id, r, 50e5)
}

// Make entries ~50B in size, to emulate real-world high cardinality.
const (
	postingsBenchSuffix = "aaaaaaaaaabbbbbbbbbbccccccccccdddddddddd"
)

func uploadTestBlock(t testing.TB, tmpDir string, bkt objstore.Bucket, series int) ulid.ULID {
	h, err := tsdb.NewHead(nil, nil, nil, 1000)
	testutil.Ok(t, err)
	defer func() {
		testutil.Ok(t, h.Close())
	}()

	logger := log.NewNopLogger()

	appendTestData(t, h.Appender(), series)

	testutil.Ok(t, os.MkdirAll(filepath.Join(tmpDir, "tmp"), os.ModePerm))
	id := createBlockFromHead(t, filepath.Join(tmpDir, "tmp"), h)

	_, err = metadata.InjectThanos(log.NewNopLogger(), filepath.Join(tmpDir, "tmp", id.String()), metadata.Thanos{
		Labels:     labels.Labels{{Name: "ext1", Value: "1"}}.Map(),
		Downsample: metadata.ThanosDownsample{Resolution: 0},
		Source:     metadata.TestSource,
	}, nil)
	testutil.Ok(t, err)
	testutil.Ok(t, block.Upload(context.Background(), logger, bkt, filepath.Join(tmpDir, "tmp", id.String())))

	return id
}

func appendTestData(t testing.TB, app tsdb.Appender, series int) {
	addSeries := func(l labels.Labels) {
		_, err := app.Add(l, 0, 0)
		testutil.Ok(t, err)
	}

	series = series / 5
	for n := 0; n < 10; n++ {
		for i := 0; i < series/10; i++ {
			addSeries(labels.FromStrings("i", strconv.Itoa(i)+postingsBenchSuffix, "n", strconv.Itoa(n)+postingsBenchSuffix, "j", "foo"))
			// Have some series that won't be matched, to properly test inverted matches.
			addSeries(labels.FromStrings("i", strconv.Itoa(i)+postingsBenchSuffix, "n", strconv.Itoa(n)+postingsBenchSuffix, "j", "bar"))
			addSeries(labels.FromStrings("i", strconv.Itoa(i)+postingsBenchSuffix, "n", "0_"+strconv.Itoa(n)+postingsBenchSuffix, "j", "bar"))
			addSeries(labels.FromStrings("i", strconv.Itoa(i)+postingsBenchSuffix, "n", "1_"+strconv.Itoa(n)+postingsBenchSuffix, "j", "bar"))
			addSeries(labels.FromStrings("i", strconv.Itoa(i)+postingsBenchSuffix, "n", "2_"+strconv.Itoa(n)+postingsBenchSuffix, "j", "foo"))
		}
	}
	testutil.Ok(t, app.Commit())
}

func createBlockFromHead(t testing.TB, dir string, head *tsdb.Head) ulid.ULID {
	compactor, err := tsdb.NewLeveledCompactor(context.Background(), nil, log.NewNopLogger(), []int64{1000000}, nil)
	testutil.Ok(t, err)

	testutil.Ok(t, os.MkdirAll(dir, 0777))

	// Add +1 millisecond to block maxt because block intervals are half-open: [b.MinTime, b.MaxTime).
	// Because of this block intervals are always +1 than the total samples it includes.
	ulid, err := compactor.Write(dir, head, head.MinTime(), head.MaxTime()+1, nil)
	testutil.Ok(t, err)
	return ulid
}

// Very similar benchmark to ths: https://github.com/prometheus/prometheus/blob/1d1732bc25cc4b47f513cb98009a4eb91879f175/tsdb/querier_bench_test.go#L82,
// but with postings results check when run as test.
func benchmarkExpandedPostings(
	t testutil.TB,
	bkt objstore.BucketReader,
	id ulid.ULID,
	r indexheader.Reader,
	series int,
) {
	n1 := labels.MustNewMatcher(labels.MatchEqual, "n", "1"+postingsBenchSuffix)

	jFoo := labels.MustNewMatcher(labels.MatchEqual, "j", "foo")
	jNotFoo := labels.MustNewMatcher(labels.MatchNotEqual, "j", "foo")

	iStar := labels.MustNewMatcher(labels.MatchRegexp, "i", "^.*$")
	iPlus := labels.MustNewMatcher(labels.MatchRegexp, "i", "^.+$")
	i1Plus := labels.MustNewMatcher(labels.MatchRegexp, "i", "^1.+$")
	iEmptyRe := labels.MustNewMatcher(labels.MatchRegexp, "i", "^$")
	iNotEmpty := labels.MustNewMatcher(labels.MatchNotEqual, "i", "")
	iNot2 := labels.MustNewMatcher(labels.MatchNotEqual, "n", "2"+postingsBenchSuffix)
	iNot2Star := labels.MustNewMatcher(labels.MatchNotRegexp, "i", "^2.*$")

	series = series / 5
	cases := []struct {
		name     string
		matchers []*labels.Matcher

		expectedLen int
	}{
		{`n="1"`, []*labels.Matcher{n1}, int(float64(series) * 0.2)},
		{`n="1",j="foo"`, []*labels.Matcher{n1, jFoo}, int(float64(series) * 0.1)},
		{`j="foo",n="1"`, []*labels.Matcher{jFoo, n1}, int(float64(series) * 0.1)},
		{`n="1",j!="foo"`, []*labels.Matcher{n1, jNotFoo}, int(float64(series) * 0.1)},
		{`i=~".*"`, []*labels.Matcher{iStar}, 5 * series},
		{`i=~".+"`, []*labels.Matcher{iPlus}, 5 * series},
		{`i=~""`, []*labels.Matcher{iEmptyRe}, 0},
		{`i!=""`, []*labels.Matcher{iNotEmpty}, 5 * series},
		{`n="1",i=~".*",j="foo"`, []*labels.Matcher{n1, iStar, jFoo}, int(float64(series) * 0.1)},
		{`n="1",i=~".*",i!="2",j="foo"`, []*labels.Matcher{n1, iStar, iNot2, jFoo}, int(float64(series) * 0.1)},
		{`n="1",i!=""`, []*labels.Matcher{n1, iNotEmpty}, int(float64(series) * 0.2)},
		{`n="1",i!="",j="foo"`, []*labels.Matcher{n1, iNotEmpty, jFoo}, int(float64(series) * 0.1)},
		{`n="1",i=~".+",j="foo"`, []*labels.Matcher{n1, iPlus, jFoo}, int(float64(series) * 0.1)},
		{`n="1",i=~"1.+",j="foo"`, []*labels.Matcher{n1, i1Plus, jFoo}, int(float64(series) * 0.011111)},
		{`n="1",i=~".+",i!="2",j="foo"`, []*labels.Matcher{n1, iPlus, iNot2, jFoo}, int(float64(series) * 0.1)},
		{`n="1",i=~".+",i!~"2.*",j="foo"`, []*labels.Matcher{n1, iPlus, iNot2Star, jFoo}, int(1 + float64(series)*0.088888)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t testutil.TB) {
			b := &bucketBlock{
				logger:            log.NewNopLogger(),
				indexHeaderReader: r,
				indexCache:        noopCache{},
				bkt:               bkt,
				meta:              &metadata.Meta{BlockMeta: tsdb.BlockMeta{ULID: id}},
				partitioner:       gapBasedPartitioner{maxGapSize: partitionerMaxGapSize},
			}

			indexr := newBucketIndexReader(context.Background(), b)

			t.ResetTimer()
			for i := 0; i < t.N(); i++ {
				p, err := indexr.ExpandedPostings(c.matchers)
				testutil.Ok(t, err)
				testutil.Equals(t, c.expectedLen, len(p))
			}
		})
	}
}

func newSeries(t testing.TB, lset labels.Labels, smplChunks [][]sample) storepb.Series {
	var s storepb.Series

	for _, l := range lset {
		s.Labels = append(s.Labels, storepb.Label{Name: l.Name, Value: l.Value})
	}

	for _, smpls := range smplChunks {
		c := chunkenc.NewXORChunk()
		a, err := c.Appender()
		testutil.Ok(t, err)

		for _, smpl := range smpls {
			a.Append(smpl.t, smpl.v)
		}

		ch := storepb.AggrChunk{
			MinTime: smpls[0].t,
			MaxTime: smpls[len(smpls)-1].t,
			Raw:     &storepb.Chunk{Type: storepb.Chunk_XOR, Data: c.Bytes()},
		}

		s.Chunks = append(s.Chunks, ch)
	}
	return s
}

func TestSeries(t *testing.T) {
	tb := testutil.NewTB(t)
	tb.Run("200e3SeriesWithOneSample", func(tb testutil.TB) {
		benchSeries(tb, 200e3, seriesDimension, 200e3)
	})
	tb.Run("OneSeriesWith200e3Samples", func(tb testutil.TB) {
		benchSeries(tb, 200e3, samplesDimension, 200e3)
	})
}

func BenchmarkSeries(b *testing.B) {
	tb := testutil.NewTB(b)
	tb.Run("10e6SeriesWithOneSample", func(tb testutil.TB) {
		benchSeries(tb, 10e6, seriesDimension, 1, 10, 10e1, 10e2, 10e3, 10e4, 10e5) // This is too big for my machine: 10e6.
	})
	tb.Run("OneSeriesWith100e6Samples", func(tb testutil.TB) {
		// 100e6 samples = ~17361 days with 15s scrape.
		benchSeries(tb, 100e6, samplesDimension, 1, 10, 10e1, 10e2, 10e3, 10e4, 10e5, 10e6) // This is too big for my machine: 100e6.
	})
}

func createBlockWithOneSample(t testutil.TB, dir string, blockIndex int, totalSeries int) (ulid.ULID, []storepb.Series) {
	fmt.Println("Building block with numSeries:", totalSeries)

	var series []storepb.Series
	h, err := tsdb.NewHead(nil, nil, nil, 1)
	testutil.Ok(t, err)
	defer testutil.Ok(t, h.Close())

	app := h.Appender()

	for i := 0; i < totalSeries; i++ {
		ts := int64(blockIndex*totalSeries + i)
		lbls := labels.FromStrings("foo", "bar", "i", fmt.Sprintf("%07d%s", ts, postingsBenchSuffix))
		series = append(series, newSeries(t, append(labels.Labels{{Name: "ext1", Value: "1"}}, lbls...), [][]sample{{sample{t: ts, v: 0}}}))

		_, err := app.Add(lbls, ts, 0)
		testutil.Ok(t, err)
	}
	testutil.Ok(t, app.Commit())

	return createBlockFromHead(t, dir, h), series
}

func createBlockWithOneSeries(t testutil.TB, dir string, lbls labels.Labels, blockIndex int, totalSamples int, random *rand.Rand) ulid.ULID {
	fmt.Println("Building block with one series with numSamples:", totalSamples)

	h, err := tsdb.NewHead(nil, nil, nil, int64(totalSamples))
	testutil.Ok(t, err)
	defer testutil.Ok(t, h.Close())

	app := h.Appender()

	ref, err := app.Add(lbls, int64(blockIndex*totalSamples), random.Float64())
	testutil.Ok(t, err)
	for i := 1; i < totalSamples; i++ {
		ts := int64(blockIndex*totalSamples + i)
		testutil.Ok(t, app.AddFast(ref, ts, random.Float64()))
	}
	testutil.Ok(t, app.Commit())

	return createBlockFromHead(t, dir, h)
}

type Dimension string

const (
	seriesDimension  = Dimension("series")
	samplesDimension = Dimension("samples")
)

func benchSeries(t testutil.TB, number int, dimension Dimension, cases ...int) {
	tmpDir, err := ioutil.TempDir("", "testorbench-series")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(tmpDir)) }()

	bkt, err := filesystem.NewBucket(filepath.Join(tmpDir, "bkt"))
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, bkt.Close()) }()

	var (
		logger = log.NewNopLogger()
		blocks []*bucketBlock
		series []storepb.Series
		random = rand.New(rand.NewSource(120))
	)

	numberPerBlock := number / 4
	lbls := labels.FromStrings("foo", "bar", "i", postingsBenchSuffix)
	switch dimension {
	case seriesDimension:
		series = make([]storepb.Series, 0, 4*numberPerBlock)
	case samplesDimension:
		series = []storepb.Series{newSeries(t, append(labels.Labels{{Name: "ext1", Value: "1"}}, lbls...), nil)}
	default:
		t.Fatal("unknown dimension", dimension)
	}

	thanosMeta := metadata.Thanos{
		Labels:     labels.Labels{{Name: "ext1", Value: "1"}}.Map(),
		Downsample: metadata.ThanosDownsample{Resolution: 0},
		Source:     metadata.TestSource,
	}

	var chunkPool pool.BytesPool
	chunkPool, err = pool.NewBucketedBytesPool(maxChunkSize, 50e6, 2, 100e7)
	testutil.Ok(t, err)

	if !t.IsBenchmark() {
		chunkPool = &mockedPool{parent: chunkPool}
	}

	blockDir := filepath.Join(tmpDir, "tmp")

	var preBuildBlockIDs []ulid.ULID
	// Local dev optimization to fetch those big blocks, instead of recreating.
	// We cannot really commit this to Git (2GB).
	// TODO(bwplotka): Provide them in objstore instead?.
	if t.IsBenchmark() {
		switch dimension {
		case seriesDimension:
			p := filepath.Join(".", "test-data", "10e6seriesOneSample")
			if _, err := os.Stat(p); err == nil {
				blockDir = p
			}
		case samplesDimension:
			p := filepath.Join(".", "test-data", "1series100e6Samples")
			if _, err := os.Stat(p); err == nil {
				blockDir = p
			}
		}

		info, err := ioutil.ReadDir(blockDir)
		if err == nil {
			for _, d := range info {
				if !d.IsDir() {
					continue
				}

				id, err := ulid.Parse(d.Name())
				if err != nil {
					continue
				}

				preBuildBlockIDs = append(preBuildBlockIDs, id)
			}
		}
	}

	for bi := 0; bi < 4; bi++ {
		var bSeries []storepb.Series

		var id ulid.ULID
		switch dimension {
		case seriesDimension:
			if len(preBuildBlockIDs) > 0 {
				id = preBuildBlockIDs[bi]
				fmt.Println("Using pre-build block:", id)
				break
			}
			// Create 4 blocks. Each will have numSeriesPerBlock number of series that have 1 sample only.
			// Timestamp will be counted for each new series, so each series will have unique timestamp.
			// This allows to pick time range that will correspond to number of series picked 1:1.
			id, bSeries = createBlockWithOneSample(t, blockDir, bi, numberPerBlock)
			series = append(series, bSeries...)
		case samplesDimension:
			if len(preBuildBlockIDs) > 0 {
				id = preBuildBlockIDs[bi]
				fmt.Println("Using pre-build block:", id)
			} else {
				// Create 4 blocks. Each will have numSeriesPerBlock number of series that have 1 sample only.
				// Timestamp will be counted for each new series, so each series will have unique timestamp.
				// This allows to pick time range that will correspond to number of series picked 1:1.
				id = createBlockWithOneSeries(t, blockDir, lbls, bi, numberPerBlock, random)
			}

			if !t.IsBenchmark() {
				// Reread chunks for ref.
				indexr, err := index.NewFileReader(filepath.Join(blockDir, id.String(), "index"))
				testutil.Ok(t, err)
				b, err := chunks.NewDirReader(filepath.Join(blockDir, id.String(), "chunks"), nil)
				testutil.Ok(t, err)

				k, v := index.AllPostingsKey()
				all, err := indexr.Postings(k, v)
				testutil.Ok(t, err)

				p, err := index.ExpandPostings(all)
				testutil.Ok(t, err)

				// One series expected.
				testutil.Equals(t, 1, len(p))
				l := labels.Labels{}
				chs := []chunks.Meta{}
				testutil.Ok(t, indexr.Series(p[0], &l, &chs))

				for _, c := range chs {
					raw, err := b.Chunk(c.Ref)
					testutil.Ok(t, err)

					series[0].Chunks = append(series[0].Chunks, storepb.AggrChunk{
						MaxTime: c.MaxTime,
						MinTime: c.MinTime,
						Raw: &storepb.Chunk{
							Data: raw.Bytes(),
							Type: storepb.Chunk_XOR,
						},
					})
				}
			}
		}

		meta, err := metadata.InjectThanos(log.NewNopLogger(), filepath.Join(blockDir, id.String()), thanosMeta, nil)
		testutil.Ok(t, err)
		testutil.Ok(t, block.Upload(context.Background(), logger, bkt, filepath.Join(blockDir, id.String())))

		b := &bucketBlock{
			indexCache:      noopCache{},
			logger:          logger,
			bkt:             bkt,
			meta:            meta,
			partitioner:     gapBasedPartitioner{maxGapSize: partitionerMaxGapSize},
			chunkObjs:       []string{filepath.Join(id.String(), "chunks", "000001")},
			chunkPool:       chunkPool,
			seriesRefetches: promauto.With(nil).NewCounter(prometheus.CounterOpts{}),
		}
		blocks = append(blocks, b)
	}

	store := &BucketStore{
		bkt:        bkt,
		logger:     logger,
		indexCache: noopCache{},
		metrics:    newBucketStoreMetrics(nil),
		blockSets: map[uint64]*bucketBlockSet{
			labels.Labels{{Name: "ext1", Value: "1"}}.Hash(): {blocks: [][]*bucketBlock{blocks}},
		},
		queryGate:      noopGater{},
		samplesLimiter: noopLimiter{},
	}

	for _, block := range blocks {
		block.indexHeaderReader, err = indexheader.NewBinaryReader(context.Background(), log.NewNopLogger(), bkt, tmpDir, block.meta.ULID)
		testutil.Ok(t, err)
	}

	var bCases []*benchSeriesCase
	for _, c := range cases {
		var expected []storepb.Series

		switch dimension {
		case seriesDimension:
			expected = series[:c]
		case samplesDimension:
			expected = series
		}

		bCases = append(bCases, &benchSeriesCase{
			name: fmt.Sprintf("%dof%d", c, 4*numberPerBlock),
			req: &storepb.SeriesRequest{
				MinTime: 0,
				MaxTime: int64(c) - 1,
				Matchers: []storepb.LabelMatcher{
					{Type: storepb.LabelMatcher_EQ, Name: "foo", Value: "bar"},
				},
			},
			expected: expected,
		})
	}

	fmt.Println("Starting")
	benchmarkSeries(t, store, bCases)
	if !t.IsBenchmark() {
		// Make sure the pool is correctly used. This is expected for 200k numbers.
		testutil.Equals(t, 4, int(chunkPool.(*mockedPool).gets))
		// TODO(bwplotka): This is super negative for large number of samples (1mln). Investigate.
		testutil.Equals(t, 0, int(chunkPool.(*mockedPool).balance))
		chunkPool.(*mockedPool).gets = 0

		for _, b := range blocks {
			// NOTE(bwplotka): It is 4 x 1.0 for 100mln samples. Kind of make sense: long series.
			testutil.Equals(t, 0.0, promtest.ToFloat64(b.seriesRefetches))
		}
	}
}

var _ = fakePool{}

type fakePool struct{}

func (m fakePool) Get(sz int) (*[]byte, error) {
	b := make([]byte, 0, sz)
	return &b, nil
}

func (m fakePool) Put(_ *[]byte) {}

type mockedPool struct {
	parent  pool.BytesPool
	balance uint64
	gets    uint64
}

func (m *mockedPool) Get(sz int) (*[]byte, error) {
	b, err := m.parent.Get(sz)
	if err != nil {
		return nil, err
	}
	atomic.AddUint64(&m.balance, uint64(cap(*b)))
	atomic.AddUint64(&m.gets, uint64(1))
	return b, nil
}

func (m *mockedPool) Put(b *[]byte) {
	atomic.AddUint64(&m.balance, ^uint64(cap(*b)-1))
	m.parent.Put(b)
}

type noopGater struct{}

func (noopGater) IsMyTurn(context.Context) error { return nil }
func (noopGater) Done()                          {}

type noopLimiter struct{}

func (noopLimiter) Check(uint64) error { return nil }

type benchSeriesCase struct {
	name     string
	req      *storepb.SeriesRequest
	expected []storepb.Series
}

func benchmarkSeries(t testutil.TB, store *BucketStore, cases []*benchSeriesCase) {
	for _, c := range cases {
		t.Run(c.name, func(t testutil.TB) {
			t.ResetTimer()
			for i := 0; i < t.N(); i++ {
				srv := newStoreSeriesServer(context.Background())
				testutil.Ok(t, store.Series(c.req, srv))
				testutil.Equals(t, 0, len(srv.Warnings))
				testutil.Equals(t, len(c.expected), len(srv.SeriesSet))

				if !t.IsBenchmark() {
					if len(c.expected) == 1 {
						// Chunks are not sorted within response. TODO: Investigate: Is this fine?
						sort.Slice(srv.SeriesSet[0].Chunks, func(i, j int) bool {
							return srv.SeriesSet[0].Chunks[i].MinTime < srv.SeriesSet[0].Chunks[j].MinTime
						})
					}
					// This might give unreadable output for millions of series if error.
					testutil.Equals(t, c.expected, srv.SeriesSet)
				}

			}
		})
	}
}

// Regression test against: https://github.com/thanos-io/thanos/issues/2147.
func TestSeries_OneBlock_InMemIndexCacheSegfault(t *testing.T) {
	tmpDir, err := ioutil.TempDir("", "segfault-series")
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, os.RemoveAll(tmpDir)) }()

	bkt, err := filesystem.NewBucket(filepath.Join(tmpDir, "bkt"))
	testutil.Ok(t, err)
	defer func() { testutil.Ok(t, bkt.Close()) }()

	logger := log.NewNopLogger()

	thanosMeta := metadata.Thanos{
		Labels:     labels.Labels{{Name: "ext1", Value: "1"}}.Map(),
		Downsample: metadata.ThanosDownsample{Resolution: 0},
		Source:     metadata.TestSource,
	}

	chunkPool, err := pool.NewBucketedBytesPool(maxChunkSize, 50e6, 2, 100e7)
	testutil.Ok(t, err)

	indexCache, err := storecache.NewInMemoryIndexCacheWithConfig(logger, nil, storecache.InMemoryIndexCacheConfig{
		MaxItemSize: 3000,
		// This is the exact size of cache needed for our *single request*.
		// This is limited in order to make sure we test evictions.
		MaxSize: 8889,
	})
	testutil.Ok(t, err)

	var b1 *bucketBlock

	const numSeries = 100

	// Create 4 blocks. Each will have numSeriesPerBlock number of series that have 1 sample only.
	// Timestamp will be counted for each new series, so each series will have unique timestamp.
	// This allows to pick time range that will correspond to number of series picked 1:1.
	{
		// Block 1.
		h, err := tsdb.NewHead(nil, nil, nil, 1)
		testutil.Ok(t, err)
		defer testutil.Ok(t, h.Close())

		app := h.Appender()

		for i := 0; i < numSeries; i++ {
			ts := int64(i)
			lbls := labels.FromStrings("foo", "bar", "b", "1", "i", fmt.Sprintf("%07d%s", ts, postingsBenchSuffix))

			_, err := app.Add(lbls, ts, 0)
			testutil.Ok(t, err)
		}
		testutil.Ok(t, app.Commit())

		blockDir := filepath.Join(tmpDir, "tmp")
		id := createBlockFromHead(t, blockDir, h)

		meta, err := metadata.InjectThanos(log.NewNopLogger(), filepath.Join(blockDir, id.String()), thanosMeta, nil)
		testutil.Ok(t, err)
		testutil.Ok(t, block.Upload(context.Background(), logger, bkt, filepath.Join(blockDir, id.String())))

		b1 = &bucketBlock{
			indexCache:  indexCache,
			logger:      logger,
			bkt:         bkt,
			meta:        meta,
			partitioner: gapBasedPartitioner{maxGapSize: partitionerMaxGapSize},
			chunkObjs:   []string{filepath.Join(id.String(), "chunks", "000001")},
			chunkPool:   chunkPool,
		}
		b1.indexHeaderReader, err = indexheader.NewBinaryReader(context.Background(), log.NewNopLogger(), bkt, tmpDir, b1.meta.ULID)
		testutil.Ok(t, err)
	}

	var b2 *bucketBlock
	{
		// Block 2, do not load this block yet.
		h, err := tsdb.NewHead(nil, nil, nil, 1)
		testutil.Ok(t, err)
		defer testutil.Ok(t, h.Close())

		app := h.Appender()

		for i := 0; i < numSeries; i++ {
			ts := int64(i)
			lbls := labels.FromStrings("foo", "bar", "b", "2", "i", fmt.Sprintf("%07d%s", ts, postingsBenchSuffix))

			_, err := app.Add(lbls, ts, 0)
			testutil.Ok(t, err)
		}
		testutil.Ok(t, app.Commit())

		blockDir := filepath.Join(tmpDir, "tmp2")
		id := createBlockFromHead(t, blockDir, h)

		meta, err := metadata.InjectThanos(log.NewNopLogger(), filepath.Join(blockDir, id.String()), thanosMeta, nil)
		testutil.Ok(t, err)
		testutil.Ok(t, block.Upload(context.Background(), logger, bkt, filepath.Join(blockDir, id.String())))

		b2 = &bucketBlock{
			indexCache:  indexCache,
			logger:      logger,
			bkt:         bkt,
			meta:        meta,
			partitioner: gapBasedPartitioner{maxGapSize: partitionerMaxGapSize},
			chunkObjs:   []string{filepath.Join(id.String(), "chunks", "000001")},
			chunkPool:   chunkPool,
		}
		b2.indexHeaderReader, err = indexheader.NewBinaryReader(context.Background(), log.NewNopLogger(), bkt, tmpDir, b2.meta.ULID)
		testutil.Ok(t, err)
	}

	store := &BucketStore{
		bkt:        bkt,
		logger:     logger,
		indexCache: indexCache,
		metrics:    newBucketStoreMetrics(nil),
		blockSets: map[uint64]*bucketBlockSet{
			labels.Labels{{Name: "ext1", Value: "1"}}.Hash(): {blocks: [][]*bucketBlock{{b1, b2}}},
		},
		blocks: map[ulid.ULID]*bucketBlock{
			b1.meta.ULID: b1,
			b2.meta.ULID: b2,
		},
		queryGate:      noopGater{},
		samplesLimiter: noopLimiter{},
	}

	t.Run("invoke series for one block. Fill the cache on the way.", func(t *testing.T) {
		srv := newStoreSeriesServer(context.Background())
		testutil.Ok(t, store.Series(&storepb.SeriesRequest{
			MinTime: 0,
			MaxTime: int64(numSeries) - 1,
			Matchers: []storepb.LabelMatcher{
				{Type: storepb.LabelMatcher_EQ, Name: "foo", Value: "bar"},
				{Type: storepb.LabelMatcher_EQ, Name: "b", Value: "1"},
				// This bug shows only when we use lot's of symbols for matching.
				{Type: storepb.LabelMatcher_NEQ, Name: "i", Value: ""},
			},
		}, srv))
		testutil.Equals(t, 0, len(srv.Warnings))
		testutil.Equals(t, numSeries, len(srv.SeriesSet))
	})
	t.Run("invoke series for second block. This should revoke previous cache.", func(t *testing.T) {
		srv := newStoreSeriesServer(context.Background())
		testutil.Ok(t, store.Series(&storepb.SeriesRequest{
			MinTime: 0,
			MaxTime: int64(numSeries) - 1,
			Matchers: []storepb.LabelMatcher{
				{Type: storepb.LabelMatcher_EQ, Name: "foo", Value: "bar"},
				{Type: storepb.LabelMatcher_EQ, Name: "b", Value: "2"},
				// This bug shows only when we use lot's of symbols for matching.
				{Type: storepb.LabelMatcher_NEQ, Name: "i", Value: ""},
			},
		}, srv))
		testutil.Equals(t, 0, len(srv.Warnings))
		testutil.Equals(t, numSeries, len(srv.SeriesSet))
	})
	t.Run("remove second block. Cache stays. Ask for first again.", func(t *testing.T) {
		testutil.Ok(t, store.removeBlock(b2.meta.ULID))

		srv := newStoreSeriesServer(context.Background())
		testutil.Ok(t, store.Series(&storepb.SeriesRequest{
			MinTime: 0,
			MaxTime: int64(numSeries) - 1,
			Matchers: []storepb.LabelMatcher{
				{Type: storepb.LabelMatcher_EQ, Name: "foo", Value: "bar"},
				{Type: storepb.LabelMatcher_EQ, Name: "b", Value: "1"},
				// This bug shows only when we use lot's of symbols for matching.
				{Type: storepb.LabelMatcher_NEQ, Name: "i", Value: ""},
			},
		}, srv))
		testutil.Equals(t, 0, len(srv.Warnings))
		testutil.Equals(t, numSeries, len(srv.SeriesSet))
	})
}
