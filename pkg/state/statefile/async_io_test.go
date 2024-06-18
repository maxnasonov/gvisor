// Copyright 2024 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package statefile

import (
	"bytes"
	"math/rand"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"github.com/maxnasonov/gvisor/pkg/fd"
)

func TestAsyncReader(t *testing.T) {
	// Create random data.
	const chunkSize = 4096
	const dataLen = 1024 * chunkSize
	data := make([]byte, dataLen)
	_, _ = rand.Read(data)

	// Create a temp file with the data.
	testFile, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatalf("failed to create temp source file: %v", err)
	}
	if _, err := testFile.Write(data); err != nil {
		t.Fatalf("failed to write temp source file: %v", err)
	}
	testFilePath := testFile.Name()
	if err := testFile.Close(); err != nil {
		t.Fatalf("failed to close temp source file: %v", err)
	}

	// Read the data from the file using async reads.
	sourceFD, err := fd.Open(testFilePath, unix.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("failed to open source file %q: %v", testFilePath, err)
	}
	ar := NewAsyncReader(sourceFD, 0 /* off */)
	defer ar.Close()
	p := make([]byte, dataLen)
	for i := 0; i < dataLen; i += chunkSize {
		ar.ReadAsync(p[i : i+chunkSize])
	}
	if err := ar.Wait(); err != nil {
		t.Fatalf("AsyncReader.Wait failed: %v", err)
	}
	if ret := bytes.Compare(p, data); ret != 0 {
		t.Errorf("bytes differ")
	}
}
