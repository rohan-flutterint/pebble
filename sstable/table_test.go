// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

package sstable

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/crlib/testutils/leaktest"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/bloom"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/objstorage/objstorageprovider"
	"github.com/cockroachdb/pebble/sstable/block"
	"github.com/cockroachdb/pebble/sstable/rowblk"
	"github.com/cockroachdb/pebble/vfs"
	"github.com/kr/pretty"
	"github.com/stretchr/testify/require"
)

func check(fs vfs.FS, filename string, comparer *Comparer, fp FilterPolicy) error {
	opts := ReaderOptions{
		Comparer: comparer,
	}
	if fp != nil {
		opts.Filters = map[string]FilterPolicy{
			fp.Name(): fp,
		}
	}

	f, err := fs.Open(filename)
	if err != nil {
		return err
	}

	r, err := newReader(f, opts)
	if err != nil {
		return err
	}

	// Check that each key/value pair in wordCount is also in the table.
	wordCount := hamletWordCount()
	words := make([]string, 0, len(wordCount))
	for k, v := range wordCount {
		words = append(words, k)
		// Check using Get.
		if v1, err := r.get([]byte(k)); string(v1) != string(v) || err != nil {
			return errors.Errorf("Get %q: got (%q, %v), want (%q, %v)", k, v1, err, v, error(nil))
		} else if len(v1) != cap(v1) {
			return errors.Errorf("Get %q: len(v1)=%d, cap(v1)=%d", k, len(v1), cap(v1))
		}

		// Check using SeekGE.
		iter, err := r.NewIter(NoTransforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
		if err != nil {
			return err
		}
		kv := iter.SeekGE([]byte(k), base.SeekGEFlagsNone)
		if kv == nil || string(kv.K.UserKey) != k {
			return errors.Errorf("Find %q: key was not in the table", k)
		}
		if actualValue, _, err := kv.Value(nil); err != nil {
			return err
		} else if string(actualValue) != v {
			return errors.Errorf("Find %q: got value %q, want %q", k, actualValue, v)
		}

		// Check using SeekLT.
		kv = iter.SeekLT([]byte(k), base.SeekLTFlagsNone)
		if kv == nil {
			kv = iter.First()
		} else {
			kv = iter.Next()
		}
		if string(kv.K.UserKey) != k {
			return errors.Errorf("Find %q: key was not in the table", k)
		}
		if actualValue, _, err := kv.Value(nil); err != nil {
			return err
		} else if string(actualValue) != v {
			return errors.Errorf("Find %q: got value %q, want %q", k, actualValue, v)
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}

	// Check that nonsense words are not in the table.
	for _, s := range hamletNonsenseWords {
		// Check using Get.
		if _, err := r.get([]byte(s)); err != base.ErrNotFound {
			return errors.Errorf("Get %q: got %v, want ErrNotFound", s, err)
		}

		// Check using Find.
		iter, err := r.NewIter(NoTransforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
		if err != nil {
			return err
		}
		if kv := iter.SeekGE([]byte(s), base.SeekGEFlagsNone); kv != nil && s == string(kv.K.UserKey) {
			return errors.Errorf("Find %q: unexpectedly found key in the table", s)
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}

	// Check that the number of keys >= a given start key matches the expected number.
	var countTests = []struct {
		count int
		start string
	}{
		// cat h.txt | cut -c 9- | wc -l gives 1710.
		{1710, ""},
		// cat h.txt | cut -c 9- | grep -v "^[a-b]" | wc -l gives 1522.
		{1522, "c"},
		// cat h.txt | cut -c 9- | grep -v "^[a-j]" | wc -l gives 940.
		{940, "k"},
		// cat h.txt | cut -c 9- | grep -v "^[a-x]" | wc -l gives 12.
		{12, "y"},
		// cat h.txt | cut -c 9- | grep -v "^[a-z]" | wc -l gives 0.
		{0, "~"},
	}
	for _, ct := range countTests {
		iter, err := r.NewIter(NoTransforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
		if err != nil {
			return err
		}
		n := 0
		for kv := iter.SeekGE([]byte(ct.start), base.SeekGEFlagsNone); kv != nil; kv = iter.Next() {
			n++
		}
		if n != ct.count {
			return errors.Errorf("count %q: got %d, want %d", ct.start, n, ct.count)
		}
		n = 0
		for kv := iter.Last(); kv != nil; kv = iter.Prev() {
			if bytes.Compare(kv.K.UserKey, []byte(ct.start)) < 0 {
				break
			}
			n++
		}
		if n != ct.count {
			return errors.Errorf("count %q: got %d, want %d", ct.start, n, ct.count)
		}
		if err := iter.Close(); err != nil {
			return err
		}
	}

	// Check lower/upper bounds behavior. Randomly choose a lower and upper bound
	// and then guarantee that iteration finds the expected number if entries.
	rng := rand.New(rand.NewPCG(0, uint64(time.Now().UnixNano())))
	sort.Strings(words)
	for i := 0; i < 10; i++ {
		lowerIdx := -1
		upperIdx := len(words)
		if rng.IntN(5) != 0 {
			lowerIdx = rng.IntN(len(words))
		}
		if rng.IntN(5) != 0 {
			upperIdx = rng.IntN(len(words))
		}
		if lowerIdx > upperIdx {
			lowerIdx, upperIdx = upperIdx, lowerIdx
		}

		var lower, upper []byte
		if lowerIdx >= 0 {
			lower = []byte(words[lowerIdx])
		} else {
			lowerIdx = 0
		}
		if upperIdx < len(words) {
			upper = []byte(words[upperIdx])
		}

		iter, err := r.NewIter(NoTransforms, lower, upper, AssertNoBlobHandles)
		if err != nil {
			return err
		}

		if lower == nil {
			n := 0
			for kv := iter.First(); kv != nil; kv = iter.Next() {
				n++
			}
			if expected := upperIdx; expected != n {
				return errors.Errorf("expected %d, but found %d", expected, n)
			}
		}

		if upper == nil {
			n := 0
			for kv := iter.Last(); kv != nil; kv = iter.Prev() {
				n++
			}
			if expected := len(words) - lowerIdx; expected != n {
				return errors.Errorf("expected %d, but found %d", expected, n)
			}
		}

		if lower != nil {
			n := 0
			for kv := iter.SeekGE(lower, base.SeekGEFlagsNone); kv != nil; kv = iter.Next() {
				n++
			}
			if expected := upperIdx - lowerIdx; expected != n {
				return errors.Errorf("expected %d, but found %d", expected, n)
			}
		}

		if upper != nil {
			n := 0
			for kv := iter.SeekLT(upper, base.SeekLTFlagsNone); kv != nil; kv = iter.Prev() {
				n++
			}
			if expected := upperIdx - lowerIdx; expected != n {
				return errors.Errorf("expected %d, but found %d", expected, n)
			}
		}

		if err := iter.Close(); err != nil {
			return err
		}
	}

	return r.Close()
}

func testReader(t *testing.T, filename string, comparer *Comparer, fp FilterPolicy) {
	// Check that we can read a pre-made table.
	err := check(vfs.Default, filepath.FromSlash("testdata/"+filename), comparer, fp)
	if err != nil {
		t.Error(err)
		return
	}
}

func TestReaderDefaultCompression(t *testing.T) {
	defer leaktest.AfterTest(t)()
	testReader(t, "hamlet-sst/000002.sst", nil, nil)
}

func TestReaderNoCompression(t *testing.T) {
	defer leaktest.AfterTest(t)()
	testReader(t, "h-no-compression-sst/000012.sst", nil, nil)
}

func TestReaderTableBloom(t *testing.T) {
	defer leaktest.AfterTest(t)()
	testReader(t, "h-table-bloom-no-compression-sst/000011.sst", nil, nil)
}

func TestReaderBloomUsed(t *testing.T) {
	defer leaktest.AfterTest(t)()
	wordCount := hamletWordCount()
	words := wordCount.SortedKeys()

	// wantActualNegatives is the minimum number of nonsense words (i.e. false
	// positives or true negatives) to run through our filter. Some nonsense
	// words might be rejected even before the filtering step, if they are out
	// of the [minWord, maxWord] range of keys in the table.
	wantActualNegatives := 0
	for _, s := range hamletNonsenseWords {
		if words[0] < s && s < words[len(words)-1] {
			wantActualNegatives++
		}
	}

	files := []struct {
		path     string
		comparer *Comparer
	}{
		{"h-table-bloom-no-compression-sst/000011.sst", nil},
		{"h-table-bloom-no-compression-prefix-extractor-no-whole-key-filter-sst/000013.sst", fixtureComparer},
	}
	for _, tc := range files {
		t.Run(tc.path, func(t *testing.T) {
			for _, degenerate := range []bool{false, true} {
				t.Run(fmt.Sprintf("degenerate=%t", degenerate), func(t *testing.T) {
					c := &countingFilterPolicy{
						FilterPolicy: bloom.FilterPolicy(10),
						degenerate:   degenerate,
					}
					testReader(t, tc.path, tc.comparer, c)

					if c.truePositives != len(wordCount) {
						t.Errorf("degenerate=%t: true positives: got %d, want %d", degenerate, c.truePositives, len(wordCount))
					}
					if c.falseNegatives != 0 {
						t.Errorf("degenerate=%t: false negatives: got %d, want %d", degenerate, c.falseNegatives, 0)
					}

					if got := c.falsePositives + c.trueNegatives; got < wantActualNegatives {
						t.Errorf("degenerate=%t: actual negatives (false positives + true negatives): "+
							"got %d (%d + %d), want >= %d",
							degenerate, got, c.falsePositives, c.trueNegatives, wantActualNegatives)
					}

					if !degenerate {
						// The true negative count should be much greater than the false
						// positive count.
						if c.trueNegatives < 10*c.falsePositives {
							t.Errorf("degenerate=%t: true negative to false positive ratio (%d:%d) is too small",
								degenerate, c.trueNegatives, c.falsePositives)
						}
					}
				})
			}
		})
	}
}

func TestBloomFilterFalsePositiveRate(t *testing.T) {
	defer leaktest.AfterTest(t)()
	f, err := vfs.Default.Open(filepath.FromSlash("testdata/h-table-bloom-no-compression-sst/000011.sst"))
	require.NoError(t, err)

	c := &countingFilterPolicy{
		FilterPolicy: bloom.FilterPolicy(1),
	}
	r, err := newReader(f, ReaderOptions{
		Filters: map[string]FilterPolicy{
			c.Name(): c,
		},
	})
	require.NoError(t, err)

	const n = 10000
	// key is a buffer that will be re-used for n Get calls, each with a
	// different key. The "m" in the 2-byte prefix means that the key falls in
	// the [minWord, maxWord] range and so will not be rejected prior to
	// applying the Bloom filter. The "!" in the 2-byte prefix means that the
	// key is not actually in the table. The filter will only see actual
	// negatives: false positives or true negatives.
	key := []byte("m!....")
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint32(key[2:6], uint32(i))
		r.get(key)
	}

	if c.truePositives != 0 {
		t.Errorf("true positives: got %d, want 0", c.truePositives)
	}
	if c.falseNegatives != 0 {
		t.Errorf("false negatives: got %d, want 0", c.falseNegatives)
	}
	if got := c.falsePositives + c.trueNegatives; got != n {
		t.Errorf("actual negatives (false positives + true negatives): got %d (%d + %d), want %d",
			got, c.falsePositives, c.trueNegatives, n)
	}

	// According the comments in the C++ LevelDB code, the false positive
	// rate should be approximately 1% for for bloom.FilterPolicy(10). The 10
	// was the parameter used to write the .sst file. When reading the file,
	// the 1 in the bloom.FilterPolicy(1) above doesn't matter, only the
	// bloom.FilterPolicy matters.
	if got := float64(100*c.falsePositives) / n; got < 0.2 || 5 < got {
		t.Errorf("false positive rate: got %v%%, want approximately 1%%", got)
	}

	require.NoError(t, r.Close())
}

type countingFilterPolicy struct {
	FilterPolicy
	degenerate bool

	truePositives  int
	falsePositives int
	falseNegatives int
	trueNegatives  int
}

func (c *countingFilterPolicy) MayContain(ftype FilterType, filter, key []byte) bool {
	got := true
	if c.degenerate {
		// When degenerate is true, we override the embedded FilterPolicy's
		// MayContain method to always return true. Doing so is a valid, if
		// inefficient, implementation of the FilterPolicy interface.
	} else {
		got = c.FilterPolicy.MayContain(ftype, filter, key)
	}
	wordCount := hamletWordCount()
	_, want := wordCount[string(key)]

	switch {
	case got && want:
		c.truePositives++
	case got && !want:
		c.falsePositives++
	case !got && want:
		c.falseNegatives++
	case !got && !want:
		c.trueNegatives++
	}
	return got
}

func TestWriterRoundTrip(t *testing.T) {
	defer leaktest.AfterTest(t)()
	blockSizes := []int{100, 1000, 2048, 4096, math.MaxInt32}
	for _, blockSize := range blockSizes {
		for _, indexBlockSize := range blockSizes {
			for name, fp := range map[string]FilterPolicy{
				"none":       nil,
				"bloom10bit": bloom.FilterPolicy(10),
			} {
				t.Run(fmt.Sprintf("bloom=%s", name), func(t *testing.T) {
					fs := vfs.NewMem()
					err := buildHamletTestSST(
						fs, "test.sst", block.DefaultCompression, fp, TableFilter,
						nil /* comparer */, blockSize, indexBlockSize,
					)
					require.NoError(t, err)
					// Check that we can read a freshly made table.
					require.NoError(t, check(fs, "test.sst", nil, nil))
				})
			}
		}
	}
}

func TestFinalBlockIsWritten(t *testing.T) {
	defer leaktest.AfterTest(t)()
	keys := []string{"A", "B", "C", "D", "E", "F", "G", "H", "I", "J"}
	valueLengths := []int{0, 1, 22, 28, 33, 40, 50, 61, 87, 100, 143, 200}
	xxx := bytes.Repeat([]byte("x"), valueLengths[len(valueLengths)-1])
	for _, blockSize := range []int{5, 10, 25, 50, 100} {
		for _, indexBlockSize := range []int{5, 10, 25, 50, 100, math.MaxInt32} {
			for nk := 0; nk <= len(keys); nk++ {
			loop:
				for _, vLen := range valueLengths {
					got, memFS := 0, vfs.NewMem()

					wf, err := memFS.Create("foo", vfs.WriteCategoryUnspecified)
					if err != nil {
						t.Errorf("nk=%d, vLen=%d: memFS create: %v", nk, vLen, err)
						continue
					}
					w := NewRawWriter(objstorageprovider.NewFileWritable(wf), WriterOptions{
						BlockSize:      blockSize,
						IndexBlockSize: indexBlockSize,
					})
					for _, k := range keys[:nk] {
						if err := w.Add(InternalKey{UserKey: []byte(k)}, xxx[:vLen], false /* forceObsolete */); err != nil {
							t.Errorf("nk=%d, vLen=%d: set: %v", nk, vLen, err)
							continue loop
						}
					}
					if err := w.Close(); err != nil {
						t.Errorf("nk=%d, vLen=%d: writer close: %v", nk, vLen, err)
						continue
					}

					rf, err := memFS.Open("foo")
					if err != nil {
						t.Errorf("nk=%d, vLen=%d: memFS open: %v", nk, vLen, err)
						continue
					}
					r, err := newReader(rf, ReaderOptions{})
					if err != nil {
						t.Errorf("nk=%d, vLen=%d: reader open: %v", nk, vLen, err)
					}
					iter, err := r.NewIter(NoTransforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
					require.NoError(t, err)
					for kv := iter.First(); kv != nil; kv = iter.Next() {
						got++
					}
					if err := iter.Close(); err != nil {
						t.Errorf("nk=%d, vLen=%d: Iterator close: %v", nk, vLen, err)
						continue
					}
					if err := r.Close(); err != nil {
						t.Errorf("nk=%d, vLen=%d: reader close: %v", nk, vLen, err)
						continue
					}

					if got != nk {
						t.Errorf("nk=%2d, vLen=%3d: got %2d keys, want %2d", nk, vLen, got, nk)
						continue
					}
				}
			}
		}
	}
}

func TestReaderSymtheticSeqNum(t *testing.T) {
	defer leaktest.AfterTest(t)()
	f, err := vfs.Default.Open(filepath.FromSlash("testdata/hamlet-sst/000002.sst"))
	require.NoError(t, err)

	r, err := newReader(f, ReaderOptions{})
	require.NoError(t, err)

	const syntheticSeqNum = 42
	transforms := IterTransforms{SyntheticSeqNum: syntheticSeqNum}

	iter, err := r.NewIter(transforms, nil /* lower */, nil /* upper */, AssertNoBlobHandles)
	require.NoError(t, err)
	for kv := iter.First(); kv != nil; kv = iter.Next() {
		if syntheticSeqNum != kv.K.SeqNum() {
			t.Fatalf("expected %d, but found %d", syntheticSeqNum, kv.K.SeqNum())
		}
	}
	require.NoError(t, iter.Close())
	require.NoError(t, r.Close())
}

func TestMetaIndexEntriesSorted(t *testing.T) {
	defer leaktest.AfterTest(t)()
	fs := vfs.NewMem()
	err := buildHamletTestSST(fs, "test.sst", block.DefaultCompression, nil, /* filter policy */
		TableFilter, nil, 4096, 4096)
	require.NoError(t, err)
	f, err := fs.Open("test.sst")
	require.NoError(t, err)

	r, err := newReader(f, ReaderOptions{})
	require.NoError(t, err)

	b, err := r.readMetaindexBlock(context.Background(), block.NoReadEnv, noReadHandle)
	require.NoError(t, err)
	defer b.Release()

	i, err := rowblk.NewRawIter(bytes.Compare, b.BlockData())
	require.NoError(t, err)

	var keys []string
	for valid := i.First(); valid; valid = i.Next() {
		keys = append(keys, string(i.Key().UserKey))
	}
	if !sort.StringsAreSorted(keys) {
		t.Fatalf("metaindex block out of order: %v", keys)
	}

	require.NoError(t, i.Close())
	require.NoError(t, r.Close())
}

func TestFooterRoundTrip(t *testing.T) {
	defer leaktest.AfterTest(t)()
	buf := make([]byte, 100+maxFooterLen)
	for format := TableFormatLevelDB; format < TableFormatMax; format++ {
		t.Run(fmt.Sprintf("format=%s", format), func(t *testing.T) {
			checksums := []block.ChecksumType{block.ChecksumTypeCRC32c}
			if format != TableFormatLevelDB {
				checksums = []block.ChecksumType{block.ChecksumTypeCRC32c, block.ChecksumTypeXXHash64}
			}
			for _, checksum := range checksums {
				t.Run(fmt.Sprintf("checksum=%d", checksum), func(t *testing.T) {
					footer := footer{
						format:      format,
						checksum:    checksum,
						metaindexBH: block.Handle{Offset: 1, Length: 2},
						indexBH:     block.Handle{Offset: 3, Length: 4},
					}
					for _, offset := range []int64{0, 1, 100} {
						t.Run(fmt.Sprintf("offset=%d", offset), func(t *testing.T) {
							mem := vfs.NewMem()
							f, err := mem.Create("test", vfs.WriteCategoryUnspecified)
							require.NoError(t, err)

							_, err = f.Write(buf[:offset])
							require.NoError(t, err)

							encoded := footer.encode(buf[100:])
							_, err = f.Write(encoded)
							require.NoError(t, err)
							require.NoError(t, f.Close())

							footer.footerBH.Offset = uint64(offset)
							footer.footerBH.Length = uint64(len(encoded))

							f, err = mem.Open("test")
							require.NoError(t, err)

							readable, err := NewSimpleReadable(f)
							require.NoError(t, err)

							result, err := readFooter(context.Background(), readable, nil, base.NoopLoggerAndTracer{}, 1)
							require.NoError(t, err)
							require.NoError(t, readable.Close())

							if diff := pretty.Diff(footer, result); diff != nil {
								t.Fatalf("expected %+v, but found %+v\n%s",
									footer, result, strings.Join(diff, "\n"))
							}
						})
					}
				})
			}
		})
	}
}

func TestReadFooter(t *testing.T) {
	defer leaktest.AfterTest(t)()
	encode := func(format TableFormat, checksum block.ChecksumType) string {
		f := footer{
			format:   format,
			checksum: checksum,
		}
		return string(f.encode(make([]byte, maxFooterLen)))
	}

	testCases := []struct {
		encoded  string
		expected string
	}{
		{strings.Repeat("a", minFooterLen-1), "file size is too small"},
		{strings.Repeat("a", levelDBFooterLen), "bad magic number"},
		{strings.Repeat("a", rocksDBFooterLen), "bad magic number"},
		{strings.Repeat("a", checkedPebbleDBFooterLen), "bad magic number"},
		{encode(TableFormatLevelDB, 0)[1:], "file size is too small"},
		{encode(TableFormatRocksDBv2, 0)[1:], "footer too short"},
		{encode(TableFormatRocksDBv2, block.ChecksumTypeNone), "unsupported checksum type"},
		{encode(TableFormatRocksDBv2, block.ChecksumTypeXXHash), "unsupported checksum type"},
	}
	for _, c := range testCases {
		t.Run("", func(t *testing.T) {
			mem := vfs.NewMem()
			f, err := mem.Create("test", vfs.WriteCategoryUnspecified)
			require.NoError(t, err)

			_, err = f.Write([]byte(c.encoded))
			require.NoError(t, err)
			require.NoError(t, f.Close())

			f, err = mem.Open("test")
			require.NoError(t, err)

			readable, err := NewSimpleReadable(f)
			require.NoError(t, err)

			if _, err := readFooter(context.Background(), readable, nil, base.NoopLoggerAndTracer{}, 1); err == nil {
				t.Fatalf("expected %q, but found success", c.expected)
			} else if !strings.Contains(err.Error(), c.expected) {
				t.Fatalf("expected %q, but found %v", c.expected, err)
			} else if !strings.Contains(err.Error(), "table 000001") {
				t.Fatalf("expected error to contain table number: %q", err)
			}
		})
	}
}
