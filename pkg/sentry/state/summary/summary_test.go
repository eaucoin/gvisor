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

package summary

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestDurationJSON(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		json string
	}{
		{0, "0"},
		{time.Millisecond, "1"},
		{1500 * time.Microsecond, "1.5"},
		{2 * time.Second, "2000"},
		{250 * time.Nanosecond, "0.00025"},
	} {
		b, err := json.Marshal(Duration(tc.d))
		if err != nil {
			t.Fatalf("Marshal(%v): %v", tc.d, err)
		}
		if string(b) != tc.json {
			t.Errorf("Marshal(%v) = %s, want %s", tc.d, b, tc.json)
		}
		var d Duration
		if err := json.Unmarshal(b, &d); err != nil {
			t.Fatalf("Unmarshal(%s): %v", b, err)
		}
		if time.Duration(d) != tc.d {
			t.Errorf("Unmarshal(%s) = %v, want %v", b, time.Duration(d), tc.d)
		}
	}
}

// TestCheckpointJSON checks the keys of a checkpoint's summary: those that the
// schema promises, with values that it has not set as null.
func TestCheckpointJSON(t *testing.T) {
	objects := int64(3)
	sum := Checkpoint{
		Schema: Schema,
		Image: &Image{
			Digest:      "d",
			Version:     "v",
			CPUFeatures: "fpu,sse",
			Layers:      []string{"l"},
			MemoryFiles: []MemoryFile{{LayerBytes: []uint64{4096, 0}}},
			Files:       []File{{Name: "pages.img", Bytes: 4096}},
		},
		Save: Save{
			Pause:         Duration(1500 * time.Microsecond),
			PrepareSave:   map[string]Duration{"unsavable_mappings": Duration(time.Millisecond)},
			Kernel:        Duration(2 * time.Millisecond),
			KernelObjects: &objects,
		},
		Pages: &Pages{WrittenBytes: 4096, Extents: 1},
	}
	b, err := json.Marshal(&sum)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,` +
		`"image":{"digest":"d","version":"v","cpu_features":"fpu,sse","layers":["l"],"memory_files":[{"layer_bytes":[4096,0]}],"files":[{"name":"pages.img","bytes":4096}]},` +
		`"save":{"pause_ms":1.5,"prepare_save_ms":{"unsavable_mappings":1},"cpuid_ms":0,"network_pause_ms":0,"kernel_ms":2,"split_fs_ms":null,"memory_files_ms":0,"overall_ms":0,"gc_ms":null,"gc_world_stopped_ms":null,"kernel_objects":3,"kernel_bytes":null},` +
		`"precopy":null,` +
		`"pages":{"written_bytes":4096,"precopied_bytes":0,"base_bytes":0,"unchanged_by_hash_bytes":0,"zero_bytes":0,"extents":1}}`
	if string(b) != want {
		t.Errorf("Marshal = %s\nwant %s", b, want)
	}
	var got Checkpoint
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sum) {
		t.Errorf("Unmarshal(Marshal(%+v)) = %+v", sum, got)
	}

	b, err = json.Marshal(&Precopy{Rounds: 2, Bytes: 8192, Duration: Duration(time.Millisecond), Stop: "converged", Converged: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"rounds":2,"bytes":8192,"ms":1,"stop":"converged","converged":true,"skipped":false}`; string(b) != want {
		t.Errorf("Marshal = %s\nwant %s", b, want)
	}
}

// TestWaitRestoreJSON checks the keys of a restore's summary.
func TestWaitRestoreJSON(t *testing.T) {
	sum := WaitRestore{
		Schema: Schema,
		Restore: Restore{
			KernelLoad:      Duration(time.Millisecond),
			OverallLoad:     Duration(2 * time.Millisecond),
			Restore:         Duration(3 * time.Millisecond),
			WorkingSetBytes: 4096,
		},
		Loading: []Loading{{Bytes: 1e6, Duration: Duration(time.Second), BackgroundBandwidth: 1, Waiters: 2, Waited: Duration(time.Millisecond), WaitedBytes: 4096, WaitedAny: Duration(time.Millisecond)}},
	}
	b, err := json.Marshal(&sum)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema":1,` +
		`"restore":{"boot_ms":0,"cpuid_load_ms":0,"kernel_load_ms":1,"memory_load_ms":null,"overall_load_ms":2,"restore_ms":3,"working_set_bytes":4096,"working_set_read_first_bytes":0},` +
		`"loading":[{"bytes":1000000,"ms":1000,"background_bandwidth_mb_s":1,"waiters":2,"waited_ms":1,"waited_bytes":4096,"waited_any_ms":1}]}`
	if string(b) != want {
		t.Errorf("Marshal = %s\nwant %s", b, want)
	}
	var got WaitRestore
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, sum) {
		t.Errorf("Unmarshal(Marshal(%+v)) = %+v", sum, got)
	}
}
