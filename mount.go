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
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	pagesize = 4096
)

func init() {
	pagesize = os.Getpagesize()
}

// Mount is the lingua franca of containerd. A mount represents a
// serialized mount syscall. Components either emit or consume mounts.
type Mount struct {
	// Type specifies the host-specific of the mount.
	Type string
	// Source specifies where to mount from. Depending on the host system, this
	// can be a source path or device.
	Source string
	// Options contains zero or more fstab-style mount options. Typically,
	// these are platform specific.
	Options []string
}

// All mounts all the provided mounts to the provided target
func All(mounts []Mount, target string) error {
	for _, m := range mounts {
		if err := m.Mount(target); err != nil {
			return err
		}
	}
	return nil
}

// Mount to the provided target path.
//
// If m.Type starts with "fuse." or "fuse3.", "mount.fuse" or "mount.fuse3"
// helper binary is called.
func (m *Mount) Mount(target string) (err error) {
	var (
		chdir   string
		options = m.Options
	)

	// avoid hitting one page limit of mount argument buffer
	//
	// NOTE: 512 is a buffer during pagesize check.
	if m.Type == "overlay" && optionsSize(options) >= pagesize-512 {
		chdir, options = compactLowerdirOption(options)
	}

	flags, data, _ := parseMountOptions(options)
	if len(data) > pagesize {
		return errors.New("mount options is too long")
	}

	// propagation types.
	const ptypes = unix.MS_SHARED | unix.MS_PRIVATE | unix.MS_SLAVE | unix.MS_UNBINDABLE

	// Ensure propagation type change flags aren't included in other calls.
	oflags := flags &^ ptypes

	// In the case of remounting with changed data (data != ""), need to call mount (moby/moby#34077).
	if flags&unix.MS_REMOUNT == 0 || data != "" {
		if err := mountAt(chdir, m.Source, target, m.Type, uintptr(oflags), data); err != nil {
			return err
		}
	}

	if flags&ptypes != 0 {
		// Change the propagation type.
		const pflags = ptypes | unix.MS_REC | unix.MS_SILENT
		if err := unix.Mount("", target, "", uintptr(flags&pflags), ""); err != nil {
			return err
		}
	}

	const broflags = unix.MS_BIND | unix.MS_RDONLY
	if oflags&broflags == broflags {
		// Remount the bind to apply read only.
		return unix.Mount("", target, "", uintptr(oflags|unix.MS_REMOUNT), "")
	}
	return nil
}

// Unmount the provided mount path with the flags
func Unmount(target string, flags int) error {
	if err := unmount(target, flags); err != nil && err != unix.EINVAL {
		return err
	}
	return nil
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

// longestCommonPrefix finds the longest common prefix in the string slice.
func longestCommonPrefix(strs []string) string {
	if len(strs) == 0 {
		return ""
	} else if len(strs) == 1 {
		return strs[0]
	}

	// find out the min/max value by alphabetical order
	min, max := strs[0], strs[0]
	for _, str := range strs[1:] {
		if min > str {
			min = str
		}
		if max < str {
			max = str
		}
	}

	// find out the common part between min and max
	for i := 0; i < len(min) && i < len(max); i++ {
		if min[i] != max[i] {
			return min[:i]
		}
	}
	return min
}

// copyOptions copies the options.
func copyOptions(opts []string) []string {
	if len(opts) == 0 {
		return nil
	}

	acopy := make([]string, len(opts))
	copy(acopy, opts)
	return acopy
}

// optionsSize returns the byte size of options of mount.
func optionsSize(opts []string) int {
	size := 0
	for _, opt := range opts {
		size += len(opt)
	}
	return size
}

// compactLowerdirOption updates overlay lowdir option and returns the common
// dir among all the lowdirs.
func compactLowerdirOption(opts []string) (string, []string) {
	idx, dirs := findOverlayLowerdirs(opts)
	if idx == -1 || len(dirs) == 1 {
		// no need to compact if there is only one lowerdir
		return "", opts
	}

	// find out common dir
	commondir := longestCommonPrefix(dirs)
	if commondir == "" {
		return "", opts
	}

	// NOTE: the snapshot id is based on digits.
	// in order to avoid to get snapshots/x, should be back to parent dir.
	// however, there is assumption that the common dir is ${root}/io.containerd.v1.overlayfs/snapshots.
	commondir = path.Dir(commondir)
	if commondir == "/" {
		return "", opts
	}
	commondir = commondir + "/"

	newdirs := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		newdirs = append(newdirs, dir[len(commondir):])
	}

	newopts := copyOptions(opts)
	newopts = append(newopts[:idx], newopts[idx+1:]...)
	newopts = append(newopts, fmt.Sprintf("lowerdir=%s", strings.Join(newdirs, ":")))
	return commondir, newopts
}

// findOverlayLowerdirs returns the index of lowerdir in mount's options and
// all the lowerdir target.
func findOverlayLowerdirs(opts []string) (int, []string) {
	var (
		idx    = -1
		prefix = "lowerdir="
	)

	for i, opt := range opts {
		if strings.HasPrefix(opt, prefix) {
			idx = i
			break
		}
	}

	if idx == -1 {
		return -1, nil
	}
	return idx, strings.Split(opts[idx][len(prefix):], ":")
}

// parseMountOptions takes fstab style mount options and parses them for
// use with a standard mount() syscall
func parseMountOptions(options []string) (int, string, bool) {
	var (
		flag    int
		losetup bool
		data    []string
	)
	loopOpt := "loop"
	flags := map[string]struct {
		clear bool
		flag  int
	}{
		"async":         {true, unix.MS_SYNCHRONOUS},
		"atime":         {true, unix.MS_NOATIME},
		"bind":          {false, unix.MS_BIND},
		"defaults":      {false, 0},
		"dev":           {true, unix.MS_NODEV},
		"diratime":      {true, unix.MS_NODIRATIME},
		"dirsync":       {false, unix.MS_DIRSYNC},
		"exec":          {true, unix.MS_NOEXEC},
		"mand":          {false, unix.MS_MANDLOCK},
		"noatime":       {false, unix.MS_NOATIME},
		"nodev":         {false, unix.MS_NODEV},
		"nodiratime":    {false, unix.MS_NODIRATIME},
		"noexec":        {false, unix.MS_NOEXEC},
		"nomand":        {true, unix.MS_MANDLOCK},
		"norelatime":    {true, unix.MS_RELATIME},
		"nostrictatime": {true, unix.MS_STRICTATIME},
		"nosuid":        {false, unix.MS_NOSUID},
		"rbind":         {false, unix.MS_BIND | unix.MS_REC},
		"relatime":      {false, unix.MS_RELATIME},
		"remount":       {false, unix.MS_REMOUNT},
		"ro":            {false, unix.MS_RDONLY},
		"rw":            {true, unix.MS_RDONLY},
		"strictatime":   {false, unix.MS_STRICTATIME},
		"suid":          {true, unix.MS_NOSUID},
		"sync":          {false, unix.MS_SYNCHRONOUS},
	}
	for _, o := range options {
		// If the option does not exist in the flags table or the flag
		// is not supported on the platform,
		// then it is a data value for a specific fs type
		if f, exists := flags[o]; exists && f.flag != 0 {
			if f.clear {
				flag &^= f.flag
			} else {
				flag |= f.flag
			}
		} else if o == loopOpt {
			losetup = true
		} else {
			data = append(data, o)
		}
	}
	return flag, strings.Join(data, ","), losetup
}

func unmount(target string, flags int) error {
	for i := 0; i < 50; i++ {
		if err := unix.Unmount(target, flags); err != nil {
			switch err {
			case unix.EBUSY:
				time.Sleep(50 * time.Millisecond)
				continue
			default:
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("failed to unmount target %s: %w", target, unix.EBUSY)
}
