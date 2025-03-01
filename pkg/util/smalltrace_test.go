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

package util

import (
	"strings"
	"testing"
)

// Renumber lines so they're stable no matter what changes above. (We
// could make the regexes accept any string of digits, but we also
// want to make sure that the correct line numbers get captured).
//
//line smalltrace_test.go:1000

func testSmallTrace2(t *testing.T) {
	s := GetSmallTrace(2)
	if !strings.Contains(s, "smalltrace_test.go:1009:util.testSmallTrace,smalltrace_test.go:1013:util.TestGenerateSmallTrace") {
		t.Fatalf("trace not generated properly: %q", s)
	}
}

func testSmallTrace(t *testing.T) {
	testSmallTrace2(t)
}

func TestGenerateSmallTrace(t *testing.T) {
	testSmallTrace(t)
}
