// Copyright 2026 The gVisor Authors.
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

package pgalloc

import (
	"bytes"
	"errors"
	"os"
	"testing"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
)

func TestGrowFile(t *testing.T) {
	for _, disk := range []bool{false, true} {
		name := "memfd"
		if disk {
			name = "file"
		}
		t.Run(name, func(t *testing.T) {
			var file *os.File
			if disk {
				var err error
				file, err = os.CreateTemp(t.TempDir(), "memory-file")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				fd, err := memutil.CreateMemFD("memory-file", 0)
				if err != nil {
					t.Fatal(err)
				}
				file = os.NewFile(uintptr(fd), "memory-file")
			}
			defer file.Close()
			f := MemoryFile{file: file}
			if err := f.growFile(0); err != nil {
				t.Fatal(err)
			}
			const marker = "preserved contents"
			var previous uint64
			for _, size := range []uint64{chunkSize, 2 * chunkSize, 4 * chunkSize} {
				var before unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &before); err != nil {
					t.Fatal(err)
				}
				if err := f.growFile(size); err != nil {
					t.Fatalf("growFile(%d): %v", size, err)
				}
				var st unix.Stat_t
				if err := unix.Fstat(int(file.Fd()), &st); err != nil {
					t.Fatal(err)
				}
				if st.Size != int64(size) {
					t.Fatalf("size: got %d, want %d", st.Size, size)
				}
				if !disk {
					if off, err := unix.Seek(int(file.Fd()), int64(size-chunkSize), unix.SEEK_DATA); !errors.Is(err, unix.ENXIO) {
						t.Fatalf("new tail has data at %d: got %v, want ENXIO", off, err)
					}
				}
				// Growth must not commit the sparse range or retain a tail page.
				if extra := (st.Blocks - before.Blocks) * 512; extra > int64(16*hostarch.PageSize) {
					t.Fatalf("growth retained %d additional physical bytes", extra)
				}
				if previous != 0 {
					got := make([]byte, len(marker))
					if _, err := file.ReadAt(got, int64(previous)-int64(len(marker))); err != nil {
						t.Fatal(err)
					}
					if string(got) != marker {
						t.Fatalf("previous tail: got %q, want %q", got, marker)
					}
				}
				// Check the start of each new chunk, including intermediate
				// chunks in a multi-chunk extension.
				for off := previous; off < size; off += chunkSize {
					buf := make([]byte, hostarch.PageSize)
					if _, err := file.ReadAt(buf, int64(off)); err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(buf, make([]byte, len(buf))) {
						t.Fatalf("new chunk at %d is not zero", off)
					}
				}
				buf := make([]byte, hostarch.PageSize)
				if _, err := file.ReadAt(buf, int64(size)-int64(len(buf))); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(buf, make([]byte, len(buf))) {
					t.Fatal("new tail is not zero")
				}
				if _, err := file.WriteAt([]byte(marker), int64(size)-int64(len(marker))); err != nil {
					t.Fatal(err)
				}
				previous = size
			}
		})
	}
}

func TestExtendChunksFailure(t *testing.T) {
	fd, err := memutil.CreateMemFD("memory-file", unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "memory-file")
	defer file.Close()
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_GROW); err != nil {
		t.Fatal(err)
	}
	f := MemoryFile{file: file}
	f.initFields()
	f.mu.Lock()
	err = f.extendChunksLocked(&allocState{length: uint64(hostarch.PageSize)})
	f.mu.Unlock()
	if !errors.Is(err, unix.EPERM) {
		t.Fatalf("extendChunksLocked: got %v, want EPERM", err)
	}
	if got := len(f.chunksLoad()); got != 0 {
		t.Fatalf("failed extension published %d chunks", got)
	}
}

func TestNewMemoryFileClearsContents(t *testing.T) {
	for _, content := range []string{"", "old contents"} {
		t.Run(content, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "memory-file")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := file.WriteString(content); err != nil {
				t.Fatal(err)
			}
			f, err := NewMemoryFile(file, MemoryFileOpts{DisableIMAWorkAround: true})
			if err != nil {
				t.Fatal(err)
			}
			defer f.Destroy()
			st, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if st.Size() != 0 {
				t.Fatalf("initial size: got %d, want 0", st.Size())
			}
		})
	}
}

func TestGrowthFailurePreservesAllocations(t *testing.T) {
	fd, err := memutil.CreateMemFD("memory-file", unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(fd), "memory-file")
	f, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	defer f.Destroy()
	fr, err := f.Allocate(uint64(hostarch.PageSize), AllocOpts{Mode: AllocateUncommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer f.DecRef(fr)
	const data = "preserved"
	if _, err := file.WriteAt([]byte(data), int64(fr.Start)); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, unix.F_SEAL_GROW); err != nil {
		t.Fatal(err)
	}
	if extra, err := f.Allocate(chunkSize+uint64(hostarch.PageSize), AllocOpts{Mode: AllocateUncommitted}); err == nil {
		f.DecRef(extra)
		t.Fatal("allocation succeeded despite F_SEAL_GROW")
	}
	if got := len(f.chunksLoad()); got != 1 {
		t.Fatalf("chunk count after failed growth: got %d, want 1", got)
	}
	buf := make([]byte, len(data))
	if _, err := file.ReadAt(buf, int64(fr.Start)); err != nil {
		t.Fatal(err)
	}
	if string(buf) != data {
		t.Fatalf("allocation contents: got %q, want %q", buf, data)
	}
	if !f.HasUniqueRef(fr) {
		t.Fatal("failed growth changed the existing allocation's references")
	}
}
