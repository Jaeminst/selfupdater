package selfupdate

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

var openFile = os.OpenFile

// Apply performs an update of the current executable (or opts.TargetFile, if set) with the contents of the given io.Reader.
//
// Apply performs the following actions to ensure a safe cross-platform update:
//
// 1. If configured, applies the contents of the update io.Reader as a binary patch.
//
// 2. If configured, computes the checksum of the new executable and verifies it matches.
//
// 3. If configured, verifies the signature with a public key.
//
// 4. Creates a new file, /path/to/.target.new with the TargetMode with the contents of the updated file
//
// 5. Renames /path/to/target to /path/to/.target.old
//
// 6. Renames /path/to/.target.new to /path/to/target
//
// 7. If the final rename is successful, deletes /path/to/.target.old, returns no error. On Windows,
// the removal of /path/to/target.old always fails, so instead Apply hides the old file instead.
//
// 8. If the final rename fails, attempts to roll back by renaming /path/to/.target.old
// back to /path/to/target.
//
// If the roll back operation fails, the file system is left in an inconsistent state (betweet steps 5 and 6) where
// there is no new executable file and the old executable file could not be be moved to its original location. In this
// case you should notify the user of the bad news and ask them to recover manually. Applications can determine whether
// the rollback failed by calling RollbackError, see the documentation on that function for additional detail.
//
// This function is provided for backward compatibility with go-selfupdate original package
func Apply(update io.Reader, opts Options) error {
	return apply(update, &opts)
}

func apply(update io.Reader, opts *Options) error {
	if opts.TargetMode == 0 {
		opts.TargetMode = 0755
	}

	// get target path
	var err error
	opts.TargetPath, err = opts.getPath()
	if err != nil {
		return err
	}

	var newBytes []byte
	if opts.Patcher != nil {
		if newBytes, err = opts.applyPatch(update); err != nil {
			return err
		}
	} else {
		// no patch to apply, go on through
		if newBytes, err = io.ReadAll(update); err != nil {
			return err
		}
	}

	// get the directory the executable exists in
	updateDir := filepath.Dir(opts.TargetPath)
	filename := filepath.Base(opts.TargetPath)

	// Copy the contents of newbinary to a new executable file
	newPath := filepath.Join(updateDir, fmt.Sprintf(".%s.new", filename))
	fp, err := openFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, opts.TargetMode)
	if err != nil {
		return err
	}
	os.Chmod(newPath, opts.TargetMode)
	defer fp.Close()

	_, err = io.Copy(fp, bytes.NewReader(newBytes))
	if err != nil {
		return err
	}
	//don't call fp.Sync().system power off ,file will lost
	fp.Sync()
	// if we don't call fp.Close(), windows won't let us move the new executable
	// because the file will still be "in use"
	fp.Close()

	// this is where we'll move the executable to so that we can swap in the updated replacement
	oldPath := opts.OldSavePath
	removeOld := opts.OldSavePath == ""
	if removeOld {
		oldPath = filepath.Join(updateDir, fmt.Sprintf(".%s.old", filename))
	}

	// delete any existing old exec file - this is necessary on Windows for two reasons:
	// 1. after a successful update, Windows can't remove the .old file because the process is still running
	// 2. windows rename operations fail if the destination file already exists
	_ = os.Remove(oldPath)

	// move the existing executable to a new file in the same directory
	err = os.Rename(opts.TargetPath, oldPath)
	if err != nil {
		return err
	}

	// move the new exectuable in to become the new program
	err = os.Rename(newPath, opts.TargetPath)

	if err != nil {
		// move unsuccessful
		//
		// The filesystem is now in a bad state. We have successfully
		// moved the existing binary to a new location, but we couldn't move the new
		// binary to take its place. That means there is no file where the current executable binary
		// used to be!
		// Try to rollback by restoring the old binary to its original path.
		rerr := os.Rename(oldPath, opts.TargetPath)
		if rerr != nil {
			return &rollbackErr{err, rerr}
		}

		return err
	}

	// move successful, remove the old binary if needed
	if removeOld {
		errRemove := os.Remove(oldPath)

		// windows has trouble with removing old binaries, so hide it instead
		if errRemove != nil {
			_ = hideFile(oldPath)
		}
	}

	return nil
}

// RollbackError takes an error value returned by Apply and returns the error, if any,
// that occurred when attempting to roll back from a failed update. Applications should
// always call this function on any non-nil errors returned by Apply.
//
// If no rollback was needed or if the rollback was successful, RollbackError returns nil,
// otherwise it returns the error encountered when trying to roll back.
func RollbackError(err error) error {
	if err == nil {
		return nil
	}
	if rerr, ok := err.(*rollbackErr); ok {
		return rerr.rollbackErr
	}
	return nil
}

type rollbackErr struct {
	error             // original error
	rollbackErr error // error encountered while rolling back
}

// Options give additional parameters when calling Apply
type Options struct {
	// TargetPath defines the path to the file to update.
	// The empty string means 'the executable file of the running program'.
	TargetPath string

	// Create TargetPath replacement with this file mode. If zero, defaults to 0755.
	TargetMode os.FileMode

	// If nil, treat the update as a complete replacement for the contents of the file at TargetPath.
	// If non-nil, treat the update contents as a patch and use this object to apply the patch.
	Patcher Patcher

	// Store the old executable file at this path after a successful update.
	// The empty string means the old executable file will be removed after the update.
	OldSavePath string
}

// CheckPermissions determines whether the process has the correct permissions to
// perform the requested update. If the update can proceed, it returns nil, otherwise
// it returns the error that would occur if an update were attempted.
func (o *Options) CheckPermissions() error {
	// get the directory the file exists in
	path, err := o.getPath()
	if err != nil {
		return err
	}

	fileDir := filepath.Dir(path)
	fileName := filepath.Base(path)

	// attempt to open a file in the file's directory
	newPath := filepath.Join(fileDir, fmt.Sprintf(".%s.new", fileName))
	fp, err := openFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, o.TargetMode)
	if err != nil {
		return err
	}
	fp.Close()

	_ = os.Remove(newPath)
	return nil
}

func (o *Options) getPath() (string, error) {
	if o.TargetPath == "" {
		var err error
		o.TargetPath, err = ExecutableRealPath()
		if err != nil {
			return "", fmt.Errorf("failed to get target real path: %v", err)
		}
	}

	// 기존 경로에서 Contents/MacOS가 있는지 확인
	if strings.Contains(o.TargetPath, "Contents/MacOS") {
		// Contents/MacOS 앞부분만 가져옴
		index := strings.Index(o.TargetPath, "Contents/MacOS")
		if index != -1 {
			// Contents/MacOS 앞부분만 추출
			return o.TargetPath[:index-1], nil
		}
	}
	return o.TargetPath, nil
}

func (o *Options) applyPatch(patch io.Reader) ([]byte, error) {
	// open the file to patch
	old, err := os.Open(o.TargetPath)
	if err != nil {
		return nil, err
	}
	defer old.Close()

	// apply the patch
	var applied bytes.Buffer
	if err = o.Patcher.Patch(old, &applied, patch); err != nil {
		return nil, err
	}

	return applied.Bytes(), nil
}

func applyAppDirectory(update string, opts *Options) error {
	if opts.TargetMode == 0 {
		opts.TargetMode = 0755
	}

	// get target path
	var err error
	opts.TargetPath, err = opts.getPath()
	if err != nil {
		return err
	}

	// get the directory the executable exists in
	updateDir := filepath.Dir(opts.TargetPath)
	filename := filepath.Base(opts.TargetPath)

	// this is where we'll move the executable to so that we can swap in the updated replacement
	oldPath := opts.OldSavePath
	removeOld := opts.OldSavePath == ""
	if removeOld {
		oldPath = filepath.Join(updateDir, fmt.Sprintf(".%s.old", filename))
	}

	// delete any existing old exec file - this is necessary on Windows for two reasons:
	// 1. after a successful update, Windows can't remove the .old file because the process is still running
	// 2. windows rename operations fail if the destination file already exists
	_ = os.RemoveAll(oldPath)

	// move the existing executable to a new file in the same directory
	err = os.Rename(opts.TargetPath, oldPath)
	if err != nil {
		return err
	}

	// move the new exectuable in to become the new program
	err = os.Rename(update, opts.TargetPath)

	if err != nil {
		// move unsuccessful
		//
		// The filesystem is now in a bad state. We have successfully
		// moved the existing binary to a new location, but we couldn't move the new
		// binary to take its place. That means there is no file where the current executable binary
		// used to be!
		// Try to rollback by restoring the old binary to its original path.
		rerr := os.Rename(oldPath, opts.TargetPath)
		if rerr != nil {
			return &rollbackErr{err, rerr}
		}

		return err
	}

	// move successful, remove the old binary if needed
	if removeOld {
		errRemove := os.RemoveAll(oldPath)

		// windows has trouble with removing old binaries, so hide it instead
		if errRemove != nil {
			_ = hideFile(oldPath)
		}
	}

	return nil
}
