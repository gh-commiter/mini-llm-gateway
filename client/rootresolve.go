package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const maxLinkRewrites = 40

// rootResolve walks rel through the root one component at a time and returns a root-relative path without links.
// A link or Windows junction whose absolute target lies inside the share is rewritten to the root-relative path;
// one that leaves the share is refused. Relative links are resolved the same way (so a chain of both kinds works).
// followLast=false keeps the last component as it is (delete, rename and create act on the link itself).
//
// The walk only decides which path to use. The operation that follows still goes through os.Root with the returned
// relative path, so a link that is retargeted after the walk still cannot lead out of the share. The protected-path
// policy must be applied to the returned path, not to rel.
func rootResolve(rooted *os.Root, rel string, followLast bool) (string, error) {
	rel = filepath.Clean(rel)
	if rel == "." {
		return rel, nil
	}
	if !filepath.IsLocal(rel) {
		return "", fmt.Errorf("invalid path: %s", rel)
	}
	var bases []string
	for rewrites := 0; ; rewrites++ {
		if rewrites > maxLinkRewrites {
			return "", fmt.Errorf("too many links: %s", rel)
		}
		parts := strings.Split(filepath.ToSlash(rel), "/")
		cur, again := ".", false
		for i, name := range parts {
			next := filepath.Join(cur, name)
			rest := parts[i+1:]
			if i == len(parts)-1 && !followLast {
				return next, nil
			}
			st, err := rooted.Lstat(next)
			if err != nil { // absent: the operation reports it
				return filepath.Join(append([]string{next}, rest...)...), nil
			}
			target := ""
			if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 { // Windows: a junction is irregular
				target, err = rooted.Readlink(next)
			}
			if target == "" || err != nil { // not a link
				cur = next
				continue
			}
			if runtime.GOOS == "windows" {
				target = trimFinalPrefix(target)
			}
			var np string
			switch {
			case filepath.IsAbs(target):
				if bases == nil {
					bases = rootBases(rooted)
				}
				var ok bool
				if np, ok = insideBases(bases, target); !ok {
					np, ok = insideBases(bases, realTarget(target)) // the link was made with another name of the share
				}
				if !ok {
					return "", fmt.Errorf("%s: %s", linkRefusal, next)
				}
			case filepath.VolumeName(target) != "" || strings.HasPrefix(target, `/`) || strings.HasPrefix(target, `\`):
				return "", fmt.Errorf("%s: %s", linkRefusal, next)
			default: // cur has no links, so joining is exact
				if np = filepath.Join(cur, target); !filepath.IsLocal(np) {
					return "", fmt.Errorf("%s: %s", linkRefusal, next)
				}
			}
			rel, again = filepath.Join(append([]string{np}, rest...)...), true
			break
		}
		if !again {
			return rel, nil
		}
	}
}

// rootBases: the root's own names: as opened, its real path and, on Windows, its final path.
func rootBases(rooted *os.Root) []string {
	bases := []string{rooted.Name()}
	if f, err := rooted.Open("."); err == nil {
		if p, err := mutationHandlePath(f); err == nil {
			bases = append(bases, p)
		}
		f.Close()
	}
	if p, err := finalPath(rooted.Name()); err == nil {
		bases = append(bases, p)
	}
	for i, b := range bases {
		if runtime.GOOS == "windows" {
			b = trimFinalPrefix(b)
		}
		bases[i] = filepath.Clean(b)
	}
	return bases
}

// insideBases: target (absolute, lexical) relative to a base it lies in; filepath.Rel ignores case on Windows.
func insideBases(bases []string, target string) (string, bool) {
	target = filepath.Clean(target)
	for _, b := range bases {
		if rel, err := filepath.Rel(b, target); err == nil && filepath.IsLocal(rel) || err == nil && rel == "." {
			return rel, true
		}
	}
	return "", false
}

// realTarget: target with its existing part resolved (a link made as /var/x, the share opened as /private/var/x).
// Only used to pick the path to try; the operation itself still goes through os.Root.
func realTarget(target string) string {
	rest := ""
	for p := filepath.Clean(target); ; {
		if r, err := finalPath(p); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(p)
		if parent == p {
			return target
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// relParts: root-relative path → components for the protected-path policy (none for the root itself).
func relParts(p string) []string {
	if p == "." {
		return nil
	}
	return strings.Split(filepath.ToSlash(p), "/")
}
