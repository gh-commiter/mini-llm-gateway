package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const linkRefusal = "links that lead outside the shared folder are not followed"

// Pin the parent, then check the location of that handle. Mutations use only its basename.
func mutationParent(root *os.Root, p string) (*os.Root, string, error) {
	return mutationParentFor(root, p, false)
}

// mutationParentFor: git true = direct mode, where git's own data is writable (protectedPathFor).
func mutationParentFor(root *os.Root, p string, git bool) (*os.Root, string, error) {
	if !filepath.IsLocal(p) || p == "." || protectedPathFor(filepath.ToSlash(p), git) {
		return nil, "", fmt.Errorf("protected path: %s", p)
	}
	parent, err := root.OpenRoot(filepath.Dir(p))
	if err != nil {
		return nil, "", fmt.Errorf("%w (%s)", err, linkRefusal)
	}
	fail := func(err error) (*os.Root, string, error) { parent.Close(); return nil, "", err }
	rf, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	defer rf.Close()
	pf, err := parent.Open(".")
	if err != nil {
		return fail(err)
	}
	defer pf.Close()
	rp, err := mutationHandlePath(rf)
	if err != nil {
		return fail(err)
	}
	pp, err := mutationHandlePath(pf)
	if err != nil {
		return fail(err)
	}
	rel, err := filepath.Rel(rp, pp)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) ||
		protectedPathFor(filepath.ToSlash(filepath.Join(rel, filepath.Base(p))), git) {
		return fail(fmt.Errorf("protected path: %s", p))
	}
	return parent, filepath.Base(p), nil
}

type mutationRoot struct {
	*os.Root
	git bool // direct mode (WebDAV): git's own data is writable
}

func (r mutationRoot) Mkdir(p string, mode os.FileMode) error {
	parent, name, err := mutationParentFor(r.Root, p, r.git)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Mkdir(name, mode)
}
func (r mutationRoot) MkdirAll(p string, mode os.FileMode) error {
	if p == "." {
		return nil
	}
	if !filepath.IsLocal(p) {
		return fmt.Errorf("invalid path: %s", p)
	}
	if err := r.MkdirAll(filepath.Dir(p), mode); err != nil {
		return err
	}
	err := r.Mkdir(p, mode)
	if os.IsExist(err) {
		st, e := r.Stat(p)
		if e != nil {
			return fmt.Errorf("%w (%s)", e, linkRefusal)
		}
		if st.IsDir() {
			return nil
		}
	}
	return err
}
func (r mutationRoot) Remove(p string) error {
	parent, name, err := mutationParentFor(r.Root, p, r.git)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Remove(name)
}
func (r mutationRoot) RemoveAll(p string) error {
	parent, name, err := mutationParentFor(r.Root, p, r.git)
	if err != nil {
		return err
	}
	defer parent.Close()
	if rootTreeProtected(parent, name, []string{name}, r.git) {
		return fmt.Errorf("protected tree: %s", p)
	}
	return parent.RemoveAll(name)
}
func (r mutationRoot) Rename(src, dst string) error {
	sp, sn, err := mutationParentFor(r.Root, src, r.git)
	if err != nil {
		return err
	}
	defer sp.Close()
	dp, dn, err := mutationParentFor(r.Root, dst, r.git)
	if err != nil {
		return err
	}
	defer dp.Close()
	if rootTreeProtected(sp, sn, []string{sn}, r.git) || rootTreeProtected(sp, sn, []string{dn}, r.git) {
		return fmt.Errorf("protected tree: %s", src)
	}
	sf, err := sp.Open(".")
	if err != nil {
		return err
	}
	defer sf.Close()
	df, err := dp.Open(".")
	if err != nil {
		return err
	}
	defer df.Close()
	return mutationRename(sf, sn, df, dn)
}
func (r mutationRoot) OpenFile(p string, flags int, mode os.FileMode) (*os.File, error) {
	if flags&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC) == 0 {
		return r.Root.OpenFile(p, flags, mode)
	}
	parent, name, err := mutationParentFor(r.Root, p, r.git)
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	// A final link must not redirect a mutation, even within this parent.
	if st, err := parent.Lstat(name); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refused link: %s", p)
	}
	return mutationOpenFile(parent, name, flags, mode)
}
