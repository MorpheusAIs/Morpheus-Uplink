package api

import (
	"fmt"
	"syscall"
)

const (
	diskWarnFreeBytes = 2 * 1024 * 1024 * 1024 // 2 GiB
	diskWarnFreePct   = 15.0
)

// diskPathStat is free-space for one filesystem path visible to the process.
type diskPathStat struct {
	Path    string  `json:"path"`
	Total   uint64  `json:"total"`
	Free    uint64  `json:"free"`
	Used    uint64  `json:"used"`
	FreePct float64 `json:"freePct"`
	Warn    bool    `json:"warn"`
}

// diskStat reports total/free/used bytes for path via Linux Statfs.
func diskStat(path string) (diskPathStat, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return diskPathStat{}, fmt.Errorf("statfs %s: %w", path, err)
	}
	bsize := uint64(st.Bsize)
	if bsize == 0 {
		return diskPathStat{}, fmt.Errorf("statfs %s: zero block size", path)
	}
	total := st.Blocks * bsize
	free := st.Bavail * bsize
	used := uint64(0)
	if total >= free {
		used = total - free
	}
	freePct := 0.0
	if total > 0 {
		freePct = float64(free) * 100.0 / float64(total)
	}
	warn := free < diskWarnFreeBytes || freePct < diskWarnFreePct
	return diskPathStat{
		Path:    path,
		Total:   total,
		Free:    free,
		Used:    used,
		FreePct: freePct,
		Warn:    warn,
	}, nil
}

func collectDiskStatus(root, dataDir string) map[string]any {
	paths := make([]diskPathStat, 0, 2)
	warn := false
	for _, p := range []string{root, dataDir} {
		if p == "" {
			continue
		}
		st, err := diskStat(p)
		if err != nil {
			paths = append(paths, diskPathStat{Path: p, Warn: true})
			warn = true
			continue
		}
		if st.Warn {
			warn = true
		}
		paths = append(paths, st)
	}
	return map[string]any{
		"paths": paths,
		"warn":  warn,
	}
}
