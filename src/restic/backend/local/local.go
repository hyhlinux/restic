package local

import (
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"restic"

	"restic/errors"

	"restic/backend"
	"restic/debug"
	"restic/fs"
)

// Local is a backend in a local directory.
type Local struct {
	Config
}

var _ restic.Backend = &Local{}

func paths(dir string) []string {
	return []string{
		dir,
		filepath.Join(dir, backend.Paths.Data),
		filepath.Join(dir, backend.Paths.Snapshots),
		filepath.Join(dir, backend.Paths.Index),
		filepath.Join(dir, backend.Paths.Locks),
		filepath.Join(dir, backend.Paths.Keys),
		filepath.Join(dir, backend.Paths.Temp),
	}
}

// Open opens the local backend as specified by config.
func Open(cfg Config) (*Local, error) {
	// test if all necessary dirs are there
	for _, d := range paths(cfg.Path) {
		if _, err := fs.Stat(d); err != nil {
			return nil, errors.Wrap(err, "Open")
		}
	}

	return &Local{Config: cfg}, nil
}

// Create creates all the necessary files and directories for a new local
// backend at dir. Afterwards a new config blob should be created.
func Create(cfg Config) (*Local, error) {
	// test if config file already exists
	_, err := fs.Lstat(filepath.Join(cfg.Path, backend.Paths.Config))
	if err == nil {
		return nil, errors.New("config file already exists")
	}

	// create paths for data, refs and temp
	for _, d := range paths(cfg.Path) {
		err := fs.MkdirAll(d, backend.Modes.Dir)
		if err != nil {
			return nil, errors.Wrap(err, "MkdirAll")
		}
	}

	// open backend
	return Open(cfg)
}

// Location returns this backend's location (the directory name).
func (b *Local) Location() string {
	return b.Path
}

// Construct path for given Type and name.
func filename(base string, t restic.FileType, name string) string {
	if t == restic.ConfigFile {
		return filepath.Join(base, "config")
	}

	return filepath.Join(dirname(base, t, name), name)
}

// Construct directory for given Type.
func dirname(base string, t restic.FileType, name string) string {
	var n string
	switch t {
	case restic.DataFile:
		n = backend.Paths.Data
		if len(name) > 2 {
			n = filepath.Join(n, name[:2])
		}
	case restic.SnapshotFile:
		n = backend.Paths.Snapshots
	case restic.IndexFile:
		n = backend.Paths.Index
	case restic.LockFile:
		n = backend.Paths.Locks
	case restic.KeyFile:
		n = backend.Paths.Keys
	}
	return filepath.Join(base, n)
}

// copyToTempfile saves p into a tempfile in tempdir.
func copyToTempfile(tempdir string, rd io.Reader) (filename string, err error) {
	tmpfile, err := ioutil.TempFile(tempdir, "temp-")
	if err != nil {
		return "", errors.Wrap(err, "TempFile")
	}

	_, err = io.Copy(tmpfile, rd)
	if err != nil {
		return "", errors.Wrap(err, "Write")
	}

	if err = tmpfile.Sync(); err != nil {
		return "", errors.Wrap(err, "Syncn")
	}

	err = tmpfile.Close()
	if err != nil {
		return "", errors.Wrap(err, "Close")
	}

	return tmpfile.Name(), nil
}

// Save stores data in the backend at the handle.
func (b *Local) Save(h restic.Handle, rd io.Reader) (err error) {
	debug.Log("Save %v", h)
	if err := h.Valid(); err != nil {
		return err
	}

	tmpfile, err := copyToTempfile(filepath.Join(b.Path, backend.Paths.Temp), rd)
	debug.Log("saved %v to %v", h, tmpfile)
	if err != nil {
		return err
	}

	filename := filename(b.Path, h.Type, h.Name)

	// test if new path already exists
	if _, err := fs.Stat(filename); err == nil {
		return errors.Errorf("Rename(): file %v already exists", filename)
	}

	// create directories if necessary, ignore errors
	if h.Type == restic.DataFile {
		err = fs.MkdirAll(filepath.Dir(filename), backend.Modes.Dir)
		if err != nil {
			return errors.Wrap(err, "MkdirAll")
		}
	}

	err = fs.Rename(tmpfile, filename)
	debug.Log("save %v: rename %v -> %v: %v",
		h, filepath.Base(tmpfile), filepath.Base(filename), err)

	if err != nil {
		return errors.Wrap(err, "Rename")
	}

	// set mode to read-only
	fi, err := fs.Stat(filename)
	if err != nil {
		return errors.Wrap(err, "Stat")
	}

	return setNewFileMode(filename, fi)
}

// Load returns a reader that yields the contents of the file at h at the
// given offset. If length is nonzero, only a portion of the file is
// returned. rd must be closed after use.
func (b *Local) Load(h restic.Handle, length int, offset int64) (io.ReadCloser, error) {
	debug.Log("Load %v, length %v, offset %v", h, length, offset)
	if err := h.Valid(); err != nil {
		return nil, err
	}

	if offset < 0 {
		return nil, errors.New("offset is negative")
	}

	f, err := os.Open(filename(b.Path, h.Type, h.Name))
	if err != nil {
		return nil, err
	}

	if offset > 0 {
		_, err = f.Seek(offset, 0)
		if err != nil {
			f.Close()
			return nil, err
		}
	}

	if length > 0 {
		return backend.LimitReadCloser(f, int64(length)), nil
	}

	return f, nil
}

// Stat returns information about a blob.
func (b *Local) Stat(h restic.Handle) (restic.FileInfo, error) {
	debug.Log("Stat %v", h)
	if err := h.Valid(); err != nil {
		return restic.FileInfo{}, err
	}

	fi, err := fs.Stat(filename(b.Path, h.Type, h.Name))
	if err != nil {
		return restic.FileInfo{}, errors.Wrap(err, "Stat")
	}

	return restic.FileInfo{Size: fi.Size()}, nil
}

// Test returns true if a blob of the given type and name exists in the backend.
func (b *Local) Test(h restic.Handle) (bool, error) {
	debug.Log("Test %v", h)
	_, err := fs.Stat(filename(b.Path, h.Type, h.Name))
	if err != nil {
		if os.IsNotExist(errors.Cause(err)) {
			return false, nil
		}
		return false, errors.Wrap(err, "Stat")
	}

	return true, nil
}

// Remove removes the blob with the given name and type.
func (b *Local) Remove(h restic.Handle) error {
	debug.Log("Remove %v", h)
	fn := filename(b.Path, h.Type, h.Name)

	// reset read-only flag
	err := fs.Chmod(fn, 0666)
	if err != nil {
		return errors.Wrap(err, "Chmod")
	}

	return fs.Remove(fn)
}

func isFile(fi os.FileInfo) bool {
	return fi.Mode()&(os.ModeType|os.ModeCharDevice) == 0
}

func readdir(d string) (fileInfos []os.FileInfo, err error) {
	f, e := fs.Open(d)
	if e != nil {
		return nil, errors.Wrap(e, "Open")
	}

	defer func() {
		e := f.Close()
		if err == nil {
			err = errors.Wrap(e, "Close")
		}
	}()

	return f.Readdir(-1)
}

// listDir returns a list of all files in d.
func listDir(d string) (filenames []string, err error) {
	fileInfos, err := readdir(d)
	if err != nil {
		return nil, err
	}

	for _, fi := range fileInfos {
		if isFile(fi) {
			filenames = append(filenames, fi.Name())
		}
	}

	return filenames, nil
}

// listDirs returns a list of all files in directories within d.
func listDirs(dir string) (filenames []string, err error) {
	fileInfos, err := readdir(dir)
	if err != nil {
		return nil, err
	}

	for _, fi := range fileInfos {
		if !fi.IsDir() {
			continue
		}

		files, err := listDir(filepath.Join(dir, fi.Name()))
		if err != nil {
			continue
		}

		filenames = append(filenames, files...)
	}

	return filenames, nil
}

// List returns a channel that yields all names of blobs of type t. A
// goroutine is started for this. If the channel done is closed, sending
// stops.
func (b *Local) List(t restic.FileType, done <-chan struct{}) <-chan string {
	debug.Log("List %v", t)
	lister := listDir
	if t == restic.DataFile {
		lister = listDirs
	}

	ch := make(chan string)
	items, err := lister(filepath.Join(dirname(b.Path, t, "")))
	if err != nil {
		close(ch)
		return ch
	}

	go func() {
		defer close(ch)
		for _, m := range items {
			if m == "" {
				continue
			}

			select {
			case ch <- m:
			case <-done:
				return
			}
		}
	}()

	return ch
}

// Delete removes the repository and all files.
func (b *Local) Delete() error {
	debug.Log("Delete()")
	return fs.RemoveAll(b.Path)
}

// Close closes all open files.
func (b *Local) Close() error {
	debug.Log("Close()")
	// this does not need to do anything, all open files are closed within the
	// same function.
	return nil
}
