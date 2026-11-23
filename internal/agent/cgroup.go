package agent

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cgroupStats reads container CPU and memory use straight from the kernel's
// cgroup v2 files, which costs nothing. Asking Docker for stats makes dockerd
// and containerd do work for every container on every round, and that was
// most of what TainerHub cost a server.
//
// It needs the host's /sys/fs/cgroup mounted read-only into the agent (the
// compose files mount it at /host/sys/fs/cgroup). Without it, or on cgroup v1,
// Stats falls back to the Docker API.
type cgroupStats struct {
	root     string // host cgroup v2 root as seen by the agent
	memTotal int64  // the host's memory, reported as the limit when none is set

	mu    sync.Mutex
	dirs  map[string]string    // container ID -> its cgroup directory
	last  map[string]cgroupCPU // container ID -> previous CPU reading
	sleep func(time.Duration)  // for tests
}

type cgroupCPU struct {
	usec uint64 // cpu.stat usage_usec
	at   time.Time
}

// newCgroupStats returns nil when no usable host cgroup v2 tree is mounted.
func newCgroupStats(roots ...string) *cgroupStats {
	for _, root := range roots {
		if root == "" {
			continue
		}
		// cgroup v2 has cgroup.controllers at its root; v1 doesn't.
		if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err != nil {
			continue
		}
		return &cgroupStats{
			root:     root,
			memTotal: hostMemTotal(),
			dirs:     map[string]string{},
			last:     map[string]cgroupCPU{},
			sleep:    time.Sleep,
		}
	}
	return nil
}

// dir finds a container's cgroup directory: systemd's cgroup driver (most
// Linux servers) or the cgroupfs driver (Docker Desktop, some distros).
func (c *cgroupStats) dir(id string) (string, bool) {
	c.mu.Lock()
	d, ok := c.dirs[id]
	c.mu.Unlock()
	if ok {
		return d, true
	}
	if len(id) != 64 { // only full IDs map to a directory name
		return "", false
	}
	for _, d := range []string{
		filepath.Join(c.root, "system.slice", "docker-"+id+".scope"),
		filepath.Join(c.root, "docker", id),
		filepath.Join(c.root, "docker.slice", "docker-"+id+".scope"),
	} {
		if _, err := os.Stat(filepath.Join(d, "cpu.stat")); err == nil {
			c.mu.Lock()
			c.dirs[id] = d
			c.mu.Unlock()
			return d, true
		}
	}
	return "", false
}

// Stats returns ok=false when the container's cgroup can't be read, so the
// caller can ask Docker instead.
func (c *cgroupStats) Stats(id string) (Usage, bool) {
	d, ok := c.dir(id)
	if !ok {
		return Usage{}, false
	}
	usec, err := readUsageUsec(filepath.Join(d, "cpu.stat"))
	if err != nil {
		c.forget(id) // the container stopped or was recreated
		return Usage{}, false
	}
	now := time.Now()
	c.mu.Lock()
	prev, havePrev := c.last[id]
	c.mu.Unlock()
	if !havePrev {
		// First look at this container: take a second reading shortly after
		// so the first number shown is real rather than 0%.
		c.sleep(500 * time.Millisecond)
		prev = cgroupCPU{usec: usec, at: now}
		if usec, err = readUsageUsec(filepath.Join(d, "cpu.stat")); err != nil {
			c.forget(id)
			return Usage{}, false
		}
		now = time.Now()
	}
	c.remember(id, cgroupCPU{usec: usec, at: now})

	var u Usage
	if wall := now.Sub(prev.at).Microseconds(); wall > 0 && usec > prev.usec {
		u.CPU = float64(usec-prev.usec) / float64(wall) * 100
	}
	mem, err := readUint(filepath.Join(d, "memory.current"))
	if err != nil {
		return Usage{}, false
	}
	// Like docker stats: leave out page cache the kernel can drop at will.
	if inactive := readStatKey(filepath.Join(d, "memory.stat"), "inactive_file"); mem > inactive {
		mem -= inactive
	}
	u.Mem = int64(mem)
	u.MemLimit = c.memTotal
	if lim, err := readUint(filepath.Join(d, "memory.max")); err == nil && lim > 0 && (c.memTotal == 0 || int64(lim) < c.memTotal) {
		u.MemLimit = int64(lim)
	}
	return u, true
}

func (c *cgroupStats) remember(id string, s cgroupCPU) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.last[id] = s
	for k, v := range c.last { // forget removed containers
		if time.Since(v.at) > 10*time.Minute {
			delete(c.last, k)
			delete(c.dirs, k)
		}
	}
}

func (c *cgroupStats) forget(id string) {
	c.mu.Lock()
	delete(c.dirs, id)
	delete(c.last, id)
	c.mu.Unlock()
}

func readUsageUsec(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if v, ok := statKey(b, "usage_usec"); ok {
		return v, nil
	}
	return 0, errors.New("no usage_usec in " + path)
}

// readUint reads a one-number file such as memory.current. "max" (no limit)
// is an error, so the caller keeps its default.
func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

func readStatKey(path, key string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, _ := statKey(b, key)
	return v
}

// statKey finds "key value" in a flat-keyed cgroup file.
func statKey(b []byte, key string) (uint64, bool) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if ok && k == key {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// hostMemTotal reads MemTotal from /proc/meminfo, which shows the host's
// memory even inside a container. Docker reports the same as the limit of a
// container without one.
func hostMemTotal() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "MemTotal:"); ok {
			f := strings.Fields(rest)
			if len(f) > 0 {
				kb, _ := strconv.ParseInt(f[0], 10, 64)
				return kb * 1024
			}
		}
	}
	return 0
}
