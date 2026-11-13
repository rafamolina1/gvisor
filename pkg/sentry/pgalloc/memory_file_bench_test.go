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
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/memutil"
)

func BenchmarkMemoryFileGrowth(b *testing.B) {
	for _, disk := range []bool{false, true} {
		name := "memfd"
		if disk {
			name = "file"
		}
		b.Run(name, func(b *testing.B) {
			dir := b.TempDir()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				var file *os.File
				if disk {
					var err error
					file, err = os.CreateTemp(dir, "memory-file")
					if err != nil {
						b.Fatal(err)
					}
					if err := os.Remove(file.Name()); err != nil {
						file.Close()
						b.Fatal(err)
					}
				} else {
					fd, err := memutil.CreateMemFD("memory-file", 0)
					if err != nil {
						b.Fatal(err)
					}
					file = os.NewFile(uintptr(fd), "memory-file")
				}
				f, err := NewMemoryFile(file, MemoryFileOpts{
					DelayedEviction:         DelayedEvictionDisabled,
					DisableMemoryAccounting: true,
					DiskBackedFile:          disk,
				})
				if err != nil {
					file.Close()
					b.Fatal(err)
				}
				b.StartTimer()
				fr, err := f.Allocate(4*chunkSize, AllocOpts{Mode: AllocateUncommitted})
				b.StopTimer()
				if err != nil {
					f.Destroy()
					b.Fatal(err)
				}
				f.DecRef(fr)
				f.Destroy()
			}
		})
	}
}
