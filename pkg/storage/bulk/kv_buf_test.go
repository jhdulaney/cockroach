// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package bulk

import (
	"bytes"
	"sort"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

// kvPair is a bytes -> bytes kv pair.
type kvPair struct {
	key   roachpb.Key
	value []byte
}

func makeTestData(num int) (kvs []kvPair, totalSize int) {
	kvs = make([]kvPair, num)
	r, _ := randutil.NewPseudoRand()
	alloc := make([]byte, num*500)
	randutil.ReadTestdataBytes(r, alloc)
	for i := range kvs {
		if len(alloc) < 1500 {
			const refill = 15000
			alloc = make([]byte, refill)
			randutil.ReadTestdataBytes(r, alloc)
		}
		kvs[i].key = alloc[:randutil.RandIntInRange(r, 2, 100)]
		alloc = alloc[len(kvs[i].key):]
		kvs[i].value = alloc[:randutil.RandIntInRange(r, 0, 1000)]
		alloc = alloc[len(kvs[i].value):]
		totalSize += len(kvs[i].key) + len(kvs[i].value)
	}
	return kvs, totalSize
}

func TestKvBuf(t *testing.T) {
	defer leaktest.AfterTest(t)()

	src, totalSize := makeTestData(50000)

	// Write everything to our buf.
	b := kvBuf{}
	for i := range src {
		if err := b.append(src[i].key, src[i].value); err != nil {
			t.Fatal(err)
		}
	}

	// Sanity check our buf has right size.
	if expected, actual := len(src), b.Len(); expected != actual {
		t.Fatalf("expected len %d got %d", expected, actual)
	}
	if expected, actual := totalSize+len(src)*16, b.MemSize; expected != actual {
		t.Fatalf("expected len %d got %d", expected, actual)
	}

	// Read back what we wrote.
	for i := range src {
		if expected, actual := src[i].key, b.Key(i); !bytes.Equal(expected, actual) {
			t.Fatalf("expected %s\ngot %s", expected, actual)
		}
		if expected, actual := src[i].value, b.Value(i); !bytes.Equal(expected, actual) {
			t.Fatalf("expected %s\ngot %s", expected, actual)
		}
	}
	// Sort both and then ensure they match.
	sort.Slice(src, func(i, j int) bool { return bytes.Compare(src[i].key, src[j].key) < 0 })
	sort.Sort(&b)
	for i := range src {
		if expected, actual := src[i].key, b.Key(i); !bytes.Equal(expected, actual) {
			t.Fatalf("expected %s\ngot %s", expected, actual)
		}
		if expected, actual := src[i].value, b.Value(i); !bytes.Equal(expected, actual) {
			t.Fatalf("expected %s\ngot %s", expected, actual)
		}
	}
}
