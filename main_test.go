/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/containerd/containerd/log/logtest"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/namespaces"
	"github.com/containerd/containerd/pkg/testutil"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/overlay"
	"github.com/containerd/containerd/snapshots/testsuite"
	"github.com/containerd/continuity/fs/fstest"
)

func TestMountat(t *testing.T) {
	for i := 0; i < 2; i++ {
		newSnapshotter := newSnapshotterWithOpts()
		t.Run("No."+strconv.Itoa(i), makeTest("128mountat", newSnapshotter, check128LayersMount("128mountat")))
	}
}

func newSnapshotterWithOpts(opts ...overlay.Opt) testsuite.SnapshotterFunc {
	return func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error) {
		snapshotter, err := overlay.NewSnapshotter(root, opts...)
		if err != nil {
			return nil, nil, err
		}

		return snapshotter, func() error { return snapshotter.Close() }, nil
	}
}

func makeTest(name string, snapshotterFn func(ctx context.Context, root string) (snapshots.Snapshotter, func() error, error), fn func(ctx context.Context, t *testing.T, snapshotter snapshots.Snapshotter, work string)) func(t *testing.T) {
	return func(t *testing.T) {
		t.Parallel()

		ctx := logtest.WithT(context.Background(), t)
		ctx = namespaces.WithNamespace(ctx, "testsuite")
		// Make two directories: a snapshotter root and a play area for the tests:
		//
		//      /tmp
		//              work/ -> passed to test functions
		//              root/ -> passed to snapshotter
		//
		tmpDir, err := os.MkdirTemp("", "snapshot-suite-"+name+"-")
		if err != nil {
			t.Fatal(err)
		}
		defer os.RemoveAll(tmpDir)

		root := filepath.Join(tmpDir, "root")
		if err := os.MkdirAll(root, 0777); err != nil {
			t.Fatal(err)
		}

		snapshotter, cleanup, err := snapshotterFn(ctx, root)
		if err != nil {
			t.Fatalf("Failed to initialize snapshotter: %+v", err)
		}
		defer func() {
			if cleanup != nil {
				if err := cleanup(); err != nil {
					t.Errorf("Cleanup failed: %v", err)
				}
			}
		}()

		work := filepath.Join(tmpDir, "work")
		if err := os.MkdirAll(work, 0777); err != nil {
			t.Fatal(err)
		}

		defer testutil.DumpDirOnFailure(t, tmpDir)
		fn(ctx, t, snapshotter, work)
	}
}

func check128LayersMount(name string) func(ctx context.Context, t *testing.T, snapshotter snapshots.Snapshotter, work string) {
	return func(ctx context.Context, t *testing.T, snapshotter snapshots.Snapshotter, work string) {
		lowestApply := fstest.Apply(
			fstest.CreateFile("/bottom", []byte("way at the bottom\n"), 0777),
			fstest.CreateFile("/overwriteme", []byte("FIRST!\n"), 0777),
			fstest.CreateDir("/ADDHERE", 0755),
			fstest.CreateDir("/ONLYME", 0755),
			fstest.CreateFile("/ONLYME/bottom", []byte("bye!\n"), 0777),
		)

		appliers := []fstest.Applier{lowestApply}
		for i := 1; i <= 127; i++ {
			appliers = append(appliers, fstest.Apply(
				fstest.CreateFile("/overwriteme", []byte(fmt.Sprintf("%d WAS HERE!\n", i)), 0777),
				fstest.CreateFile(fmt.Sprintf("/ADDHERE/file-%d", i), []byte("same\n"), 0755),
				fstest.RemoveAll("/ONLYME"),
				fstest.CreateDir("/ONLYME", 0755),
				fstest.CreateFile(fmt.Sprintf("/ONLYME/file-%d", i), []byte("only me!\n"), 0777),
			))
		}

		flat := filepath.Join(work, "flat")
		if err := os.MkdirAll(flat, 0777); err != nil {
			t.Fatalf("failed to create flat dir(%s): %+v", flat, err)
		}

		// NOTE: add gc labels to avoid snapshots get removed by gc...
		parent := ""
		for i, applier := range appliers {
			preparing := filepath.Join(work, fmt.Sprintf("prepare-layer-%d", i))
			if err := os.MkdirAll(preparing, 0777); err != nil {
				t.Fatalf("[layer %d] failed to create preparing dir(%s): %+v", i, preparing, err)
			}

			dmounts, err := snapshotter.Prepare(ctx, preparing, parent, opt)
			if err != nil {
				t.Fatalf("[layer %d] failed to get mount info: %+v", i, err)
			}

			mounts := fromContainerd(dmounts)
			// FOCUS <---
			if err := All(mounts, preparing); err != nil {
				t.Fatalf("[layer %d] failed to mount on the target(%s): %+v", i, preparing, err)
			}

			if err := fstest.CheckDirectoryEqual(preparing, flat); err != nil {
				testutil.Unmount(t, preparing)
				t.Fatalf("[layer %d] preparing doesn't equal to flat before apply: %+v", i, err)
			}

			if err := applier.Apply(flat); err != nil {
				testutil.Unmount(t, preparing)
				t.Fatalf("[layer %d] failed to apply on flat dir: %+v", i, err)
			}

			if err = applier.Apply(preparing); err != nil {
				testutil.Unmount(t, preparing)
				t.Fatalf("[layer %d] failed to apply on preparing dir: %+v", i, err)
			}

			if err := fstest.CheckDirectoryEqual(preparing, flat); err != nil {
				testutil.Unmount(t, preparing)
				t.Fatalf("[layer %d] preparing doesn't equal to flat after apply: %+v", i, err)
			}

			testutil.Unmount(t, preparing)

			parent = filepath.Join(work, fmt.Sprintf("committed-%d", i))
			if err := snapshotter.Commit(ctx, parent, preparing, opt); err != nil {
				t.Fatalf("[layer %d] failed to commit the preparing: %+v", i, err)
			}

		}

		view := filepath.Join(work, "fullview")
		if err := os.MkdirAll(view, 0777); err != nil {
			t.Fatalf("failed to create fullview dir(%s): %+v", view, err)
		}

		dmounts, err := snapshotter.View(ctx, view, parent, opt)
		if err != nil {
			t.Fatalf("failed to get view's mount info: %+v", err)
		}

		// FOCUS
		mounts := fromContainerd(dmounts)
		if err := All(mounts, view); err != nil {
			t.Fatalf("failed to mount on the target(%s): %+v", view, err)
		}
		defer testutil.Unmount(t, view)

		if err := fstest.CheckDirectoryEqual(view, flat); err != nil {
			t.Fatalf("fullview should equal to flat: %+v", err)
		}
	}
}

func fromContainerd(mounts []mount.Mount) []Mount {
	res := make([]Mount, len(mounts))
	for i, m := range mounts {
		res[i].Type = m.Type
		res[i].Source = m.Source
		res[i].Options = m.Options
	}
	return res
}

var opt = snapshots.WithLabels(map[string]string{
	"containerd.io/gc.root": time.Now().UTC().Format(time.RFC3339),
})
