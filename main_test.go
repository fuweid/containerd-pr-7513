package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountat(t *testing.T) {
	t.Parallel()

	for i := 0; i < 2000; i++ {
		t.Run("No."+strconv.Itoa(i), func(t *testing.T) {
			testOSLockThreadForMountat(t)
		})
	}
}

func testOSLockThreadForMountat(t *testing.T) {
	t.Parallel()

	dir1 := t.TempDir()
	dir2 := t.TempDir()

	defer unix.Unmount(filepath.Join(dir2, "bar"), unix.MNT_DETACH)

	if err := os.WriteFile(filepath.Join(dir1, "foo"), []byte("foo"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir2, "bar"), []byte{}, 0644); err != nil {
		t.Fatal(err)
	}

	// mount ${dir1}/foo at ${dir2}/bar
	// But since we are using `mountAt` we only need to specify the relative path to dir2 as the target mountAt will chdir to there.
	if err := mountAt(dir2, filepath.Join(dir1, "foo"), "bar", "none", unix.MS_BIND, ""); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(dir2, "bar"))
	if err != nil {
		t.Fatal(err)
	}

	if string(b) != "foo" {
		t.Fatalf("unexpected file content: %s", b)
	}
}

func mountAt(chdir string, source, target, fstype string, flags uintptr, data string) error {
	if chdir == "" {
		return unix.Mount(source, target, fstype, flags, data)
	}

	ch := make(chan error, 1)
	go func() {
		runtime.LockOSThread()

		// Do not unlock this thread.
		// If the thread is unlocked go will try to use it for other goroutines.
		// However it is not possible to restore the thread state after CLONE_FS.
		//
		// Once the goroutine exits the thread should eventually be terminated by go.

		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			ch <- err
			return
		}

		if err := unix.Chdir(chdir); err != nil {
			ch <- err
			return
		}

		ch <- unix.Mount(source, target, fstype, flags, data)
	}()
	return <-ch
}
