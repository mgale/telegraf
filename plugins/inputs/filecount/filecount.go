//go:generate ../../../tools/readme_config_includer/generator
package filecount

import (
	_ "embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/karrick/godirwalk"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/internal/globpath"
	"github.com/influxdata/telegraf/plugins/inputs"
)

//go:embed sample.conf
var sampleConfig string

type FileCount struct {
	Directories    []string        `toml:"directories"`
	Name           string          `toml:"name"`
	Recursive      bool            `toml:"recursive"`
	RegularOnly    bool            `toml:"regular_only"`
	FollowSymlinks bool            `toml:"follow_symlinks"`
	Size           config.Size     `toml:"size"`
	MTime          config.Duration `toml:"mtime"`
	Log            telegraf.Logger `toml:"-"`

	fs          fileSystem
	fileFilters []fileFilterFunc
	globPaths   []globpath.GlobPath
}

type fileFilterFunc func(os.FileInfo) (bool, error)

func (*FileCount) SampleConfig() string {
	return sampleConfig
}

func (fc *FileCount) Gather(acc telegraf.Accumulator) error {
	if fc.globPaths == nil {
		fc.initGlobPaths(acc)
	}

	for _, glob := range fc.globPaths {
		for _, dir := range fc.onlyDirectories(glob.GetRoots()) {
			fc.count(acc, dir, glob)
		}
	}

	return nil
}

func rejectNilFilters(filters []fileFilterFunc) []fileFilterFunc {
	filtered := make([]fileFilterFunc, 0, len(filters))
	for _, f := range filters {
		if f != nil {
			filtered = append(filtered, f)
		}
	}
	return filtered
}

func (fc *FileCount) nameFilter() fileFilterFunc {
	if fc.Name == "*" {
		return nil
	}

	return func(f os.FileInfo) (bool, error) {
		match, err := filepath.Match(fc.Name, f.Name())
		if err != nil {
			return false, err
		}
		return match, nil
	}
}

func (fc *FileCount) regularOnlyFilter() fileFilterFunc {
	if !fc.RegularOnly {
		return nil
	}

	return func(f os.FileInfo) (bool, error) {
		return f.Mode().IsRegular(), nil
	}
}

func (fc *FileCount) sizeFilter() fileFilterFunc {
	if fc.Size == 0 {
		return nil
	}

	return func(f os.FileInfo) (bool, error) {
		if !f.Mode().IsRegular() {
			return false, nil
		}
		if fc.Size < 0 {
			return f.Size() < -int64(fc.Size), nil
		}
		return f.Size() >= int64(fc.Size), nil
	}
}

func (fc *FileCount) mtimeFilter() fileFilterFunc {
	if time.Duration(fc.MTime) == 0 {
		return nil
	}

	return func(f os.FileInfo) (bool, error) {
		age := absDuration(time.Duration(fc.MTime))
		mtime := time.Now().Add(-age)
		if time.Duration(fc.MTime) < 0 {
			return f.ModTime().After(mtime), nil
		}
		return f.ModTime().Before(mtime), nil
	}
}

func absDuration(x time.Duration) time.Duration {
	if x < 0 {
		return -x
	}
	return x
}

func (fc *FileCount) initFileFilters() {
	filters := []fileFilterFunc{
		fc.nameFilter(),
		fc.regularOnlyFilter(),
		fc.sizeFilter(),
		fc.mtimeFilter(),
	}
	fc.fileFilters = rejectNilFilters(filters)
}

func (fc *FileCount) count(acc telegraf.Accumulator, basedir string, glob globpath.GlobPath) {
	childCount := make(map[string]int64)
	childSize := make(map[string]int64)
	oldestFileTimestamp := make(map[string]int64)
	newestFileTimestamp := make(map[string]int64)

	walkFn := func(path string, _ *godirwalk.Dirent) error {
		rel, err := filepath.Rel(basedir, path)
		if err == nil && rel == "." {
			return nil
		}
		file, err := fc.resolveLink(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		match, err := fc.filter(file)
		if err != nil {
			acc.AddError(err)
			return nil
		}
		if match {
			parent := filepath.Dir(path)
			childCount[parent]++
			childSize[parent] += file.Size()
			if oldestFileTimestamp[parent] == 0 || oldestFileTimestamp[parent] > file.ModTime().UnixNano() {
				oldestFileTimestamp[parent] = file.ModTime().UnixNano()
			}
			if newestFileTimestamp[parent] == 0 || newestFileTimestamp[parent] < file.ModTime().UnixNano() {
				newestFileTimestamp[parent] = file.ModTime().UnixNano()
			}
		}
		if file.IsDir() && !fc.Recursive && !glob.HasSuperMeta {
			return filepath.SkipDir
		}
		return nil
	}

	postChildrenFn := func(path string, _ *godirwalk.Dirent) error {
		if glob.MatchString(path) {
			gauge := map[string]interface{}{
				"count":      childCount[path],
				"size_bytes": childSize[path],
			}
			gauge["oldest_file_timestamp"] = oldestFileTimestamp[path]
			gauge["newest_file_timestamp"] = newestFileTimestamp[path]
			acc.AddGauge("filecount", gauge,
				map[string]string{
					"directory": path,
				})
		}
		parent := filepath.Dir(path)
		if fc.Recursive {
			childCount[parent] += childCount[path]
			childSize[parent] += childSize[path]
			if oldestFileTimestamp[parent] == 0 || oldestFileTimestamp[parent] > oldestFileTimestamp[path] {
				oldestFileTimestamp[parent] = oldestFileTimestamp[path]
			}
			if newestFileTimestamp[parent] == 0 || newestFileTimestamp[parent] < newestFileTimestamp[path] {
				newestFileTimestamp[parent] = newestFileTimestamp[path]
			}
		}
		delete(childCount, path)
		delete(childSize, path)
		delete(oldestFileTimestamp, path)
		delete(newestFileTimestamp, path)
		return nil
	}

	err := godirwalk.Walk(basedir, &godirwalk.Options{
		Callback:             walkFn,
		PostChildrenCallback: postChildrenFn,
		Unsorted:             true,
		FollowSymbolicLinks:  fc.FollowSymlinks,
		ErrorCallback: func(_ string, err error) godirwalk.ErrorAction {
			if errors.Is(err, fs.ErrPermission) {
				fc.Log.Debug(err)
				return godirwalk.SkipNode
			}
			return godirwalk.Halt
		},
	})
	if err != nil {
		acc.AddError(err)
	}
}

func (fc *FileCount) filter(file os.FileInfo) (bool, error) {
	if fc.fileFilters == nil {
		fc.initFileFilters()
	}

	for _, fileFilter := range fc.fileFilters {
		match, err := fileFilter(file)
		if err != nil {
			return false, err
		}
		if !match {
			return false, nil
		}
	}

	return true, nil
}

func (fc *FileCount) resolveLink(path string) (os.FileInfo, error) {
	if fc.FollowSymlinks {
		return fc.fs.stat(path)
	}
	fi, err := fc.fs.lstat(path)
	if err != nil {
		return fi, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		// if this file is a symlink, skip it
		return nil, godirwalk.SkipThis
	}
	return fi, nil
}

func (fc *FileCount) onlyDirectories(directories []string) []string {
	out := make([]string, 0)
	for _, path := range directories {
		info, err := fc.fs.stat(path)
		if err == nil && info.IsDir() {
			out = append(out, path)
		}
	}
	return out
}

func (fc *FileCount) getDirs() []string {
	dirs := make([]string, 0, len(fc.Directories)+1)
	for _, dir := range fc.Directories {
		dirs = append(dirs, filepath.Clean(dir))
	}

	return dirs
}

func (fc *FileCount) initGlobPaths(acc telegraf.Accumulator) {
	dirs := fc.getDirs()
	fc.globPaths = make([]globpath.GlobPath, 0, len(dirs))
	for _, directory := range dirs {
		glob, err := globpath.Compile(directory)
		if err != nil {
			acc.AddError(err)
		} else {
			fc.globPaths = append(fc.globPaths, *glob)
		}
	}
}

func newFileCount() *FileCount {
	return &FileCount{
		Name:           "*",
		Recursive:      true,
		RegularOnly:    true,
		FollowSymlinks: false,
		Size:           config.Size(0),
		MTime:          config.Duration(0),
		fileFilters:    nil,
		fs:             osFS{},
	}
}

func init() {
	inputs.Add("filecount", func() telegraf.Input {
		return newFileCount()
	})
}
