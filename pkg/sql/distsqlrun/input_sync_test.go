// Copyright 2016 The Cockroach Authors.
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

package distsqlrun

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/distsqlpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/pkg/errors"
)

func TestOrderedSync(t *testing.T) {
	defer leaktest.AfterTest(t)()

	v := [6]sqlbase.EncDatum{}
	for i := range v {
		v[i] = sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(i)))
	}

	asc := encoding.Ascending
	desc := encoding.Descending

	testCases := []struct {
		sources  []sqlbase.EncDatumRows
		ordering sqlbase.ColumnOrdering
		expected sqlbase.EncDatumRows
	}{
		{
			sources: []sqlbase.EncDatumRows{
				{
					{v[0], v[1], v[4]},
					{v[0], v[1], v[2]},
					{v[0], v[2], v[3]},
					{v[1], v[1], v[3]},
				},
				{
					{v[1], v[0], v[4]},
				},
				{
					{v[0], v[0], v[0]},
					{v[4], v[4], v[4]},
				},
			},
			ordering: sqlbase.ColumnOrdering{
				{ColIdx: 0, Direction: asc},
				{ColIdx: 1, Direction: asc},
			},
			expected: sqlbase.EncDatumRows{
				{v[0], v[0], v[0]},
				{v[0], v[1], v[4]},
				{v[0], v[1], v[2]},
				{v[0], v[2], v[3]},
				{v[1], v[0], v[4]},
				{v[1], v[1], v[3]},
				{v[4], v[4], v[4]},
			},
		},
		{
			sources: []sqlbase.EncDatumRows{
				{},
				{
					{v[1], v[0], v[4]},
				},
				{
					{v[3], v[4], v[1]},
					{v[4], v[4], v[4]},
					{v[3], v[2], v[0]},
				},
				{
					{v[4], v[4], v[5]},
					{v[3], v[3], v[0]},
					{v[0], v[0], v[0]},
				},
			},
			ordering: sqlbase.ColumnOrdering{
				{ColIdx: 1, Direction: desc},
				{ColIdx: 0, Direction: asc},
				{ColIdx: 2, Direction: asc},
			},
			expected: sqlbase.EncDatumRows{
				{v[3], v[4], v[1]},
				{v[4], v[4], v[4]},
				{v[4], v[4], v[5]},
				{v[3], v[3], v[0]},
				{v[3], v[2], v[0]},
				{v[0], v[0], v[0]},
				{v[1], v[0], v[4]},
			},
		},
	}
	for testIdx, c := range testCases {
		var sources []RowSource
		for _, srcRows := range c.sources {
			rowBuf := NewRowBuffer(sqlbase.ThreeIntCols, srcRows, RowBufferArgs{})
			sources = append(sources, rowBuf)
		}
		evalCtx := tree.NewTestingEvalContext(cluster.MakeTestingClusterSettings())
		defer evalCtx.Stop(context.Background())
		src, err := makeOrderedSync(c.ordering, evalCtx, sources)
		if err != nil {
			t.Fatal(err)
		}
		src.Start(context.Background())
		var retRows sqlbase.EncDatumRows
		for {
			row, meta := src.Next()
			if meta != nil {
				t.Fatalf("unexpected metadata: %v", meta)
			}
			if row == nil {
				break
			}
			retRows = append(retRows, row)
		}
		expStr := c.expected.String(sqlbase.ThreeIntCols)
		retStr := retRows.String(sqlbase.ThreeIntCols)
		if expStr != retStr {
			t.Errorf("invalid results for case %d; expected:\n   %s\ngot:\n   %s",
				testIdx, expStr, retStr)
		}
	}
}

func TestOrderedSyncDrainBeforeNext(t *testing.T) {
	defer leaktest.AfterTest(t)()

	expectedMeta := &distsqlpb.ProducerMetadata{Err: errors.New("expected metadata")}

	var sources []RowSource
	for i := 0; i < 4; i++ {
		rowBuf := NewRowBuffer(sqlbase.OneIntCol, nil /* rows */, RowBufferArgs{})
		sources = append(sources, rowBuf)
		rowBuf.Push(nil, expectedMeta)
	}

	ctx := context.Background()
	evalCtx := tree.NewTestingEvalContext(cluster.MakeTestingClusterSettings())
	defer evalCtx.Stop(ctx)
	o, err := makeOrderedSync(sqlbase.ColumnOrdering{}, evalCtx, sources)
	if err != nil {
		t.Fatal(err)
	}
	o.Start(ctx)

	// Call ConsumerDone before Next has been called.
	o.ConsumerDone()

	metasFound := 0
	for {
		_, meta := o.Next()
		if meta == nil {
			break
		}

		if meta != expectedMeta {
			t.Fatalf("unexpected meta %v, expected %v", meta, expectedMeta)
		}

		metasFound++
	}
	if metasFound != len(sources) {
		t.Fatalf("unexpected number of metadata items %d, expected %d", metasFound, len(sources))
	}
}

func TestUnorderedSync(t *testing.T) {
	defer leaktest.AfterTest(t)()

	mrc := &RowChannel{}
	mrc.InitWithNumSenders([]types.T{*types.Int}, 5)
	producerErr := make(chan error, 100)
	for i := 1; i <= 5; i++ {
		go func(i int) {
			for j := 1; j <= 100; j++ {
				a := sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(i)))
				b := sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(j)))
				row := sqlbase.EncDatumRow{a, b}
				if status := mrc.Push(row, nil /* meta */); status != NeedMoreRows {
					producerErr <- errors.Errorf("producer error: unexpected response: %d", status)
				}
			}
			mrc.ProducerDone()
		}(i)
	}
	var retRows sqlbase.EncDatumRows
	for {
		row, meta := mrc.Next()
		if meta != nil {
			t.Fatalf("unexpected metadata: %v", meta)
		}
		if row == nil {
			break
		}
		retRows = append(retRows, row)
	}
	// Verify all elements.
	for i := 1; i <= 5; i++ {
		j := 1
		for _, row := range retRows {
			if int(tree.MustBeDInt(row[0].Datum)) == i {
				if int(tree.MustBeDInt(row[1].Datum)) != j {
					t.Errorf("Expected [%d %d], got %s", i, j, row.String(sqlbase.TwoIntCols))
				}
				j++
			}
		}
		if j != 101 {
			t.Errorf("Missing [%d %d]", i, j)
		}
	}
	select {
	case err := <-producerErr:
		t.Fatal(err)
	default:
	}

	// Test case when one source closes with an error.
	mrc = &RowChannel{}
	mrc.InitWithNumSenders([]types.T{*types.Int}, 5)
	for i := 1; i <= 5; i++ {
		go func(i int) {
			for j := 1; j <= 100; j++ {
				a := sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(i)))
				b := sqlbase.DatumToEncDatum(types.Int, tree.NewDInt(tree.DInt(j)))
				row := sqlbase.EncDatumRow{a, b}
				if status := mrc.Push(row, nil /* meta */); status != NeedMoreRows {
					producerErr <- errors.Errorf("producer error: unexpected response: %d", status)
				}
			}
			if i == 3 {
				err := fmt.Errorf("Test error")
				mrc.Push(nil /* row */, &distsqlpb.ProducerMetadata{Err: err})
			}
			mrc.ProducerDone()
		}(i)
	}
	foundErr := false
	for {
		row, meta := mrc.Next()
		if meta != nil && meta.Err != nil {
			if meta.Err.Error() != "Test error" {
				t.Error(meta.Err)
			} else {
				foundErr = true
			}
		}
		if row == nil && meta == nil {
			break
		}
	}
	select {
	case err := <-producerErr:
		t.Fatal(err)
	default:
	}
	if !foundErr {
		t.Error("Did not receive expected error")
	}
}
