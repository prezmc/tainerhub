package agent

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/prezmc/tainerhub/internal/wire"
)

// manifestFile lists the extra files (Dockerfile, scripts, config) the last
// deploy wrote into the stack folder, one path per line. Only files listed
// there are ever removed, so data and files someone put in the folder by
// hand are left alone.
const manifestFile = ".tainerhub-files"

// Stack files are written readable: a build copies them into an image
// (keeping their mode), and a container may read a mounted config file as
// its own user. The agent creates stack folders 0700, which keeps other
// users on the host out.
const (
	stackFileMode = 0o644
	stackExecMode = 0o755
)

// readManifest returns the paths the last deploy wrote. Anything in it that
// isn't a valid stack file path is ignored, so an edited manifest can't
// point the agent outside the stack folder.
func readManifest(dir string) []string {
	p := filepath.Join(dir, manifestFile)
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && wire.ValidStackFilePath(line) == nil {
			out = append(out, line)
		}
	}
	return out
}

func manifestContent(files []wire.StackFile) []byte {
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("# Files TainerHub wrote for this stack. It removes them when they leave the stack; other files here are left alone.\n")
	for _, p := range paths {
		b.WriteString(p + "\n")
	}
	return []byte(b.String())
}

// writeNoFollow creates a new file without following a symlink at path.
func writeNoFollow(path string, content []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil { // the umask may have taken bits off
		f.Close()
		return err
	}
	return f.Close()
}

// stageFiles writes the stack's extra files into the (fresh) staging folder.
func stageFiles(staging string, files []wire.StackFile) error {
	for _, f := range files {
		p := filepath.Join(staging, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		mode := os.FileMode(stackFileMode)
		if f.Executable {
			mode = stackExecMode
		}
		if err := writeNoFollow(p, f.Content, mode); err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}

// errSymlink is returned for a path that goes through a symbolic link: the
// agent never writes or deletes through one, since it could point anywhere
// on the host.
type errSymlink struct{ path string }

func (e errSymlink) Error() string {
	return e.path + " is a symbolic link on the host; TainerHub won't write or delete through it. Remove the link, then deploy again."
}

// walkParents checks each folder above rel inside dir: none may be a
// symlink or a file. With create, missing folders are made; without, the
// first missing one ends the walk (returns missing=true).
func walkParents(dir, rel string, create bool) (missing bool, err error) {
	parts := strings.Split(rel, "/")
	cur := dir
	for i, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		shown := strings.Join(parts[:i+1], "/")
		st, err := os.Lstat(cur)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if !create {
				return true, nil
			}
			if err := os.Mkdir(cur, 0o755); err != nil {
				return false, err
			}
		case err != nil:
			return false, err
		case st.Mode()&fs.ModeSymlink != 0:
			return false, errSymlink{shown}
		case !st.IsDir():
			return false, fmt.Errorf("%s is a file on the host, but %s needs it to be a folder", shown, rel)
		}
	}
	return false, nil
}

// checkTarget refuses to replace a symlink or a folder with a stack file.
func checkTarget(dir, rel string) error {
	st, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(rel)))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case st.Mode()&fs.ModeSymlink != 0:
		return errSymlink{rel}
	case st.IsDir():
		return fmt.Errorf("%s is a folder on the host; remove or rename it to add a file by that name", rel)
	}
	return nil
}

// placeFiles moves the staged extra files into the stack folder, then
// writes the new manifest. Every path is checked before anything moves, so
// a refused path leaves the folder as it was.
func placeFiles(staging, dir string, files []wire.StackFile) error {
	for _, f := range files {
		missing, err := walkParents(dir, f.Path, false)
		if err != nil {
			return err
		}
		if !missing {
			if err := checkTarget(dir, f.Path); err != nil {
				return err
			}
		}
	}
	if err := checkTarget(dir, manifestFile); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := walkParents(dir, f.Path, true); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(staging, filepath.FromSlash(f.Path)), filepath.Join(dir, filepath.FromSlash(f.Path))); err != nil {
			return err
		}
	}
	tmp := filepath.Join(staging, manifestFile)
	if err := writeNoFollow(tmp, manifestContent(files), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, manifestFile))
}

// removeStale deletes files an earlier deploy wrote that are no longer part
// of the stack (old lists the manifest's paths), and then folders that only
// held them. Symlinks are never followed or removed; a file that can't be
// removed is reported, not fatal: the stack itself is deployed.
func removeStale(dir string, old []string, keep []wire.StackFile, out io.Writer) {
	kept := map[string]bool{}
	for _, f := range keep {
		kept[f.Path] = true
	}
	for _, rel := range old {
		if kept[rel] {
			continue
		}
		if err := removeManaged(dir, rel); err != nil {
			fmt.Fprintf(out, "Couldn't remove %s, which is no longer part of the stack: %v\n", rel, err)
		}
	}
}

// removeManaged removes one file TainerHub wrote, then its folders while
// they are empty.
func removeManaged(dir, rel string) error {
	missing, err := walkParents(dir, rel, false)
	if err != nil || missing {
		return err
	}
	p := filepath.Join(dir, filepath.FromSlash(rel))
	st, err := os.Lstat(p)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case st.Mode()&fs.ModeSymlink != 0:
		return errSymlink{rel}
	case !st.Mode().IsRegular():
		return errors.New("it isn't a regular file any more; left as it is")
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	for d := filepath.Dir(rel); d != "." && d != "/"; d = filepath.Dir(d) {
		// Only succeeds while empty: a folder that also holds other files stays.
		if os.Remove(filepath.Join(dir, filepath.FromSlash(d))) != nil {
			break
		}
	}
	return nil
}

// removeAllManaged removes every file the manifest lists, and the manifest.
func removeAllManaged(dir string, out io.Writer) {
	removeStale(dir, readManifest(dir), nil, out)
	if st, err := os.Lstat(filepath.Join(dir, manifestFile)); err == nil && st.Mode().IsRegular() {
		_ = os.Remove(filepath.Join(dir, manifestFile))
	}
}
