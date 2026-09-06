//go:build linux

package persistenceowner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type ownershipPlan struct {
	start    string
	leafOnly bool
}

type repairer struct {
	uid int
	gid int

	fchown       func(fd, uid, gid int) error
	afterOpenDir func(path string) error
}

func newRepairer(uid, gid int) *repairer {
	return &repairer{
		uid:     uid,
		gid:     gid,
		fchown: syscall.Fchown,
	}
}

func Repair(cfg Config) error {
	return repairWith(cfg, newRepairer(cfg.UID, cfg.GID))
}

func repairWith(cfg Config, r *repairer) error {
	if cfg.UID < 0 || cfg.GID < 0 {
		return fmt.Errorf("invalid persistence ownership uid/gid: %d:%d", cfg.UID, cfg.GID)
	}
	if r == nil || r.fchown == nil {
		return errors.New("persistence ownership repairer is not configured")
	}

	cwd, err := resolveWorkingDir(cfg.WorkingDir)
	if err != nil {
		return err
	}
	dataRootRaw := strings.TrimSpace(cfg.DataRoot)
	if dataRootRaw == "" {
		dataRootRaw = "/data"
	}
	dataRoot, err := normalizeConfiguredPath(dataRootRaw, cwd)
	if err != nil {
		return fmt.Errorf("normalize data root: %w", err)
	}
	dataRoots := uniquePaths(dataRoot, filepath.Join(cwd, "data"))

	dbRaw := strings.TrimSpace(cfg.DBPath)
	if dbRaw == "" {
		dbRaw = filepath.Join(dataRoot, "servers.db")
	}
	dbPath, err := normalizeConfiguredPath(dbRaw, cwd)
	if err != nil {
		return fmt.Errorf("normalize database path: %w", err)
	}
	dbDir := filepath.Dir(dbPath)

	// Preserve upgrade compatibility for the canonical Docker data root without
	// ever recursively traversing app-writable content.
	dataPlan := ownershipPlan{start: dataRoot}
	if err := r.repairDirectory(dataRoot, false, dataPlan); err != nil {
		if !errors.Is(err, syscall.ENOENT) {
			return fmt.Errorf("repair data root: %w", err)
		}
	} else if err := r.repairFile(filepath.Join(dataRoot, "servers.json"), false, dataPlan); err != nil {
		return fmt.Errorf("repair legacy servers file: %w", err)
	}

	dbPlan := directoryOwnershipPlan(dbDir, dataRoots, "")
	if err := r.repairDirectory(dbDir, true, dbPlan); err != nil {
		return fmt.Errorf("repair database directory: %w", err)
	}
	for _, path := range []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		dbPath + "-journal",
		filepath.Join(dbDir, "config.json"),
	} {
		if err := r.repairFile(path, false, dbPlan); err != nil {
			return fmt.Errorf("repair persistence file %q: %w", path, err)
		}
	}

	knownHostsRaw := firstConfiguredPath(cfg.KnownHosts)
	knownHostsConfigured := knownHostsRaw != ""
	if !knownHostsConfigured {
		knownHostsRaw = filepath.Join(dbDir, "known_hosts")
	}
	knownHostsPath, err := normalizeConfiguredPath(knownHostsRaw, cwd)
	if err != nil {
		return fmt.Errorf("normalize known-hosts path: %w", err)
	}
	if knownHostsConfigured && !shouldRepairKnownHosts(knownHostsPath, dbDir, dataRoots) {
		return nil
	}
	knownHostsDir := filepath.Dir(knownHostsPath)
	knownHostsPlan := directoryOwnershipPlan(knownHostsDir, dataRoots, dbDir)
	if err := r.repairDirectory(knownHostsDir, true, knownHostsPlan); err != nil {
		return fmt.Errorf("repair known-hosts directory: %w", err)
	}
	if err := r.repairFile(knownHostsPath, false, knownHostsPlan); err != nil {
		return fmt.Errorf("repair known-hosts file: %w", err)
	}
	return nil
}

func resolveWorkingDir(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("resolve working directory: %w", err)
		}
		return filepath.Clean(cwd), nil
	}
	cwd := strings.TrimSpace(raw)
	if !filepath.IsAbs(cwd) {
		abs, err := filepath.Abs(cwd)
		if err != nil {
			return "", fmt.Errorf("resolve working directory %q: %w", raw, err)
		}
		cwd = abs
	}
	return filepath.Clean(cwd), nil
}

func normalizeConfiguredPath(raw, cwd string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("persistence path is empty")
	}
	if strings.IndexByte(trimmed, 0) >= 0 {
		return "", errors.New("persistence path contains NUL")
	}
	for _, component := range strings.Split(trimmed, string(os.PathSeparator)) {
		if component == ".." {
			return "", fmt.Errorf("persistence path %q contains '..'", trimmed)
		}
	}
	clean := filepath.Clean(trimmed)
	if clean == "." {
		return "", fmt.Errorf("persistence path %q does not name a file or directory", trimmed)
	}
	if !filepath.IsAbs(clean) {
		clean = filepath.Join(cwd, clean)
	}
	return filepath.Clean(clean), nil
}

func firstConfiguredPath(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	for _, candidate := range filepath.SplitList(raw) {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			return candidate
		}
	}
	return ""
}

func uniquePaths(paths ...string) []string {
	seen := make(map[string]struct{}, len(paths))
	result := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func shouldRepairKnownHosts(path, dbDir string, dataRoots []string) bool {
	for _, root := range dataRoots {
		if pathWithin(root, path) {
			return true
		}
	}
	return pathWithin(dbDir, path)
}

func directoryOwnershipPlan(dir string, dataRoots []string, fallbackRoot string) ownershipPlan {
	for _, root := range dataRoots {
		if pathWithin(root, dir) {
			return ownershipPlan{start: root}
		}
	}
	if fallbackRoot != "" && pathWithin(fallbackRoot, dir) {
		return ownershipPlan{start: fallbackRoot}
	}
	return ownershipPlan{leafOnly: true}
}

func pathWithin(base, target string) bool {
	base = filepath.Clean(base)
	target = filepath.Clean(target)
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func (p ownershipPlan) shouldChown(path string, leaf bool) bool {
	if p.leafOnly {
		return leaf
	}
	return p.start != "" && pathWithin(p.start, path)
}

func (r *repairer) repairDirectory(path string, create bool, plan ownershipPlan) error {
	fd, err := r.openDirectory(path, create, plan)
	if err != nil {
		return err
	}
	return syscall.Close(fd)
}

func (r *repairer) openDirectory(path string, create bool, plan ownershipPlan) (int, error) {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return -1, fmt.Errorf("internal error: directory path %q is not absolute", clean)
	}
	if clean == string(os.PathSeparator) {
		return -1, errors.New("refusing to change ownership of filesystem root")
	}

	components := strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator))
	currentFD, err := syscall.Open(string(os.PathSeparator), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("open filesystem root: %w", err)
	}
	currentPath := string(os.PathSeparator)

	for i, component := range components {
		if component == "" || component == "." || component == ".." {
			syscall.Close(currentFD)
			return -1, fmt.Errorf("invalid persistence path component %q in %q", component, clean)
		}

		createdComponent := false
		nextFD, openErr := syscall.Openat(currentFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(openErr, syscall.ENOENT) && create {
			mkdirErr := syscall.Mkdirat(currentFD, component, 0o700)
			if mkdirErr == nil {
				createdComponent = true
			} else if !errors.Is(mkdirErr, syscall.EEXIST) {
				syscall.Close(currentFD)
				return -1, fmt.Errorf("create persistence directory %q: %w", filepath.Join(currentPath, component), mkdirErr)
			}
			nextFD, openErr = syscall.Openat(currentFD, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		if openErr != nil {
			syscall.Close(currentFD)
			return -1, describeOpenError(filepath.Join(currentPath, component), openErr)
		}

		nextPath := filepath.Join(currentPath, component)
		if r.afterOpenDir != nil {
			if hookErr := r.afterOpenDir(nextPath); hookErr != nil {
				syscall.Close(nextFD)
				syscall.Close(currentFD)
				return -1, hookErr
			}
		}
		if createdComponent || plan.shouldChown(nextPath, i == len(components)-1) {
			if err := r.fchown(nextFD, r.uid, r.gid); err != nil {
				syscall.Close(nextFD)
				syscall.Close(currentFD)
				return -1, fmt.Errorf("fchown persistence directory %q: %w", nextPath, err)
			}
		}
		syscall.Close(currentFD)
		currentFD = nextFD
		currentPath = nextPath
	}
	return currentFD, nil
}

func (r *repairer) repairFile(path string, createParent bool, plan ownershipPlan) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("internal error: file path %q is not absolute", clean)
	}
	name := filepath.Base(clean)
	if name == "." || name == string(os.PathSeparator) {
		return fmt.Errorf("invalid persistence file path %q", clean)
	}

	parentFD, err := r.openDirectory(filepath.Dir(clean), createParent, plan)
	if err != nil {
		return err
	}
	defer syscall.Close(parentFD)

	fileFD, err := syscall.Openat(parentFD, name, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if errors.Is(err, syscall.ENOENT) {
		return nil
	}
	if err != nil {
		return describeOpenError(clean, err)
	}
	defer syscall.Close(fileFD)

	var stat syscall.Stat_t
	if err := syscall.Fstat(fileFD, &stat); err != nil {
		return fmt.Errorf("inspect persistence file %q: %w", clean, err)
	}
	if stat.Mode&syscall.S_IFMT != syscall.S_IFREG {
		return fmt.Errorf("expected regular persistence file: %s", clean)
	}
	if err := r.fchown(fileFD, r.uid, r.gid); err != nil {
		return fmt.Errorf("fchown persistence file %q: %w", clean, err)
	}
	return nil
}

func describeOpenError(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.ENOTDIR) {
		return fmt.Errorf("refusing symbolic link or non-directory persistence component %q: %w", path, err)
	}
	return fmt.Errorf("open persistence path %q: %w", path, err)
}

// descriptorPath is used only by Linux regression tests to identify the object
// referenced by a descriptor after a concurrent rename.
func descriptorPath(fd int) (string, error) {
	return os.Readlink(filepath.Join("/proc/self/fd", strconv.Itoa(fd)))
}
