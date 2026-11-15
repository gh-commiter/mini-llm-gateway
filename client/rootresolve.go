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
	// realpath semantics: cur is always a link-free root-relative path, queue the components still to walk, so a ..
	// after a link applies to the link's target (never cleaned away as text).
	var bases []string
	queue, cur, lastLink := relParts(rel), ".", rel
	for rewrites := 0; len(queue) > 0; {
		name := queue[0]
		queue = queue[1:]
		switch name {
		case "", ".":
			continue
		case "..":
			if cur == "." {
				return "", fmt.Errorf("%s: %s", linkRefusal, lastLink)
			}
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		if len(queue) == 0 && !followLast {
			return next, nil
		}
		st, err := rooted.Lstat(next)
		if err != nil { // absent: the operation reports it
			for _, part := range queue {
				if part == ".." {
					return "", fmt.Errorf("%s: %s", linkRefusal, lastLink)
				}
			}
			return filepath.Join(append([]string{next}, queue...)...), nil
		}
		target := ""
		if st.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 { // Windows: a junction is irregular
			target, err = rooted.Readlink(next)
		}
		if target == "" || err != nil { // not a link
			cur = next
			continue
		}
		if rewrites++; rewrites > maxLinkRewrites {
			return "", fmt.Errorf("too many links: %s", rel)
		}
		if runtime.GOOS == "windows" {
			target = trimFinalPrefix(target)
		}
		lastLink = next
		switch {
		case filepath.IsAbs(target):
			if bases == nil {
				bases = rootBases(rooted)
			}
			np, ok := insideLinkBases(bases, target)
			if !ok {
				np, ok = insideLinkBases(bases, realTarget(target, bases)) // the link was made with another name of the share
			}
			if !ok {
				return "", fmt.Errorf("%s: %s", linkRefusal, next)
			}
			cur, queue = ".", append(relParts(np), queue...)
		case filepath.VolumeName(target) != "" || strings.HasPrefix(target, `/`) || strings.HasPrefix(target, `\`):
			return "", fmt.Errorf("%s: %s", linkRefusal, next)
		default: // relative to the link's folder, which is cur
			queue = append(relParts(target), queue...)
		}
	}
	return cur, nil
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

// finalPathFn: test hook (an untrusted UNC path must never reach it).
var finalPathFn = finalPath

// sameVolume: target is on the volume or drive of one of the root's own names (compared lexically, nothing is opened).
// A link may name any absolute path; resolving a UNC or device path would connect to that host (L2).
func sameVolume(bases []string, target string) bool {
	v := filepath.VolumeName(target)
	for _, b := range bases {
		if strings.EqualFold(filepath.VolumeName(b), v) {
			return true
		}
	}
	return false
}

// safeFinalPath inspects each link before opening its target, including intermediate links and chains.
// The caller supplies trusted base names; UNC paths may probe only inside those bases.
func safeFinalPath(target string, bases []string) (string, error) {
	p := target
	if !filepath.IsAbs(p) {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		p = wd + string(filepath.Separator) + p
	}
	for rewrites := 0; rewrites <= maxLinkRewrites; rewrites++ {
		trustedUNC := trustedUNCPath(p, bases)
		if !trustedUNC && !safeTarget(p, bases) {
			return "", fmt.Errorf("%s: %s", linkRefusal, target)
		}
		volume := filepath.VolumeName(p)
		cur := volume + string(filepath.Separator)
		parts := strings.Split(strings.TrimPrefix(p, cur), string(filepath.Separator))
		again := false
		for i, name := range parts {
			if name == "" {
				continue
			}
			if name == ".." {
				cur = filepath.Dir(cur)
				continue
			}
			cur = filepath.Join(cur, name)
			if !trustedUNC && !safeTarget(cur, bases) {
				return "", fmt.Errorf("%s: %s", linkRefusal, target)
			}
			st, err := os.Lstat(cur)
			if err != nil {
				return "", err
			}
			if st.Mode()&(os.ModeSymlink|os.ModeIrregular) == 0 {
				continue
			}
			next, err := os.Readlink(cur)
			if err != nil {
				return "", err
			}
			if runtime.GOOS == "windows" {
				next = trimFinalPrefix(next)
			}
			if !filepath.IsAbs(next) {
				if filepath.VolumeName(next) != "" || strings.HasPrefix(next, `\`) || strings.HasPrefix(next, "/") {
					return "", fmt.Errorf("%s: %s", linkRefusal, target)
				}
				next = filepath.Dir(cur) + string(filepath.Separator) + next
			}
			if !trustedUNCPath(next, bases) && !safeTarget(next, bases) {
				return "", fmt.Errorf("%s: %s", linkRefusal, target)
			}
			p = strings.Join(append([]string{next}, parts[i+1:]...), string(filepath.Separator))
			again = true
			break
		}
		if !again {
			return finalPathFn(cur)
		}
	}
	return "", fmt.Errorf("too many links: %s", target)
}

// trustedUNCPath checks containment before any probe of a UNC target, including link rewrites.
func trustedUNCPath(target string, bases []string) bool {
	if runtime.GOOS != "windows" || !strings.HasPrefix(filepath.VolumeName(target), `\\`) {
		return false
	}
	_, inside := insideBases(bases, target)
	return inside
}

func safeTarget(target string, bases []string) bool {
	if runtime.GOOS == "windows" {
		v := filepath.VolumeName(target)
		return len(v) == 2 && v[1] == ':' && sameVolume(bases, target)
	}
	p := filepath.Clean(target)
	if runtime.GOOS == "darwin" {
		p = strings.ToLower(p)
		if p == "/net" || strings.HasPrefix(p, "/net/") || p == "/network" || strings.HasPrefix(p, "/network/") {
			return false
		}
		return safeTargetFilesystem(target)
	}
	return p != "/net" && !strings.HasPrefix(p, "/net/") && p != "/Network" && !strings.HasPrefix(p, "/Network/") && safeTargetFilesystem(target)
}

// realTarget: target with its existing part resolved (a link made as /var/x, the share opened as /private/var/x).
// Only used to pick the path to try; the operation itself still goes through os.Root. A target on another volume is
// returned as it is (then it is outside the root).
func realTarget(target string, bases []string) string {
	if !safeTarget(target, bases) {
		return target
	}
	rest := ""
	for p := target; ; {
		if r, err := safeFinalPath(p, bases); err == nil {
			for _, part := range strings.Split(filepath.ToSlash(rest), "/") {
				if part == ".." {
					return target
				}
			}
			return filepath.Join(r, rest)
		} else if !os.IsNotExist(err) {
			return target
		}
		if filepath.Base(p) == ".." {
			return target
		}
		parent := filepath.Dir(p)
		// Removing a missing suffix must not clean .. across an unresolved link.
		if i := strings.LastIndexAny(p, `/\`); i > len(filepath.VolumeName(p)) {
			parent = p[:i]
		}
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

// Walk from the root and stop at a mount before probing any of its children.
var targetAutofs = isAutofs

func safeTargetFilesystem(target string) bool {
	parts := strings.Split(target, "/")
	cur, rewrites := "/", 0
	for len(parts) > 0 {
		mounted, err := targetAutofs(cur)
		if err != nil || mounted {
			return false
		}
		name := parts[0]
		parts = parts[1:]
		if name == "" || name == "." {
			continue
		}
		if name == ".." {
			cur = filepath.Dir(cur)
			continue
		}
		next := filepath.Join(cur, name)
		st, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil {
			return false
		}
		if st.Mode()&os.ModeSymlink != 0 {
			rewrites++
			if rewrites > maxLinkRewrites {
				return false
			}
			link, err := os.Readlink(next)
			if err != nil {
				return false
			}
			if filepath.IsAbs(link) {
				cur = "/"
			}
			parts = append(strings.Split(link, "/"), parts...)
		} else {
			cur = next
		}
	}
	mounted, err := targetAutofs(cur)
	return err == nil && !mounted
}

// Preserve link-target components; the rooted walk applies .. after resolving links.
func insideLinkBases(bases []string, target string) (string, bool) {
	for _, base := range bases {
		b := filepath.Clean(base)
		prefix := strings.TrimRight(b, string(filepath.Separator)) + string(filepath.Separator)
		t, p := target, prefix
		if runtime.GOOS == "windows" {
			t, p = strings.ToLower(t), strings.ToLower(p)
		}
		if strings.HasPrefix(t, p) {
			return target[len(prefix):], true
		}
		if t == strings.TrimSuffix(p, string(filepath.Separator)) {
			return ".", true
		}
	}
	return "", false
}
