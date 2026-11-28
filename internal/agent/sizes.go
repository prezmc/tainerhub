package agent

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/prezmc/tainerhub/internal/wire"
)

// Measuring disk use makes Docker walk each container's writable layer and
// each volume, which can take a while on a busy host. So the agent measures
// on its own slow timer (see sizesLoop) and List only reads the cache.

// sizeCache holds the last measured disk use per container ID.
type sizeCache struct {
	mu    sync.Mutex
	at    time.Time
	sizes map[string]wire.ContainerSize
}

func (c *sizeCache) get(id string) *wire.ContainerSize {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sizes[id]
	if !ok {
		return nil
	}
	return &s
}

func (c *sizeCache) set(sizes map[string]wire.ContainerSize, at time.Time) {
	c.mu.Lock()
	c.sizes, c.at = sizes, at
	c.mu.Unlock()
}

// fresh: measured less than maxAge ago.
func (c *sizeCache) fresh(maxAge time.Duration, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.at.IsZero() && now.Sub(c.at) < maxAge
}

// dfReport is the part of Docker's /system/df answer used for sizes.
type dfReport struct {
	Containers []struct {
		ID     string `json:"Id"`
		SizeRw int64
		Mounts []struct {
			Type string
			Name string
		}
	}
	Volumes []struct {
		Name      string
		Driver    string
		Options   map[string]string
		UsageData *struct {
			Size int64 // -1 when Docker didn't measure it
		}
	}
}

// dfSizes works out each container's sizes from a /system/df report.
func dfSizes(r dfReport, at time.Time) map[string]wire.ContainerSize {
	vols := map[string]int64{}
	for _, v := range r.Volumes {
		// A network share's size is the remote server's business, and
		// walking it would be slow: leave it out.
		if v.UsageData == nil || v.UsageData.Size < 0 || v.Driver != "local" || wire.ShareFromOptions(v.Driver, v.Options) != nil {
			continue
		}
		vols[v.Name] = v.UsageData.Size
	}
	out := make(map[string]wire.ContainerSize, len(r.Containers))
	for _, c := range r.Containers {
		s := wire.ContainerSize{RW: max(c.SizeRw, 0), At: at.Unix()}
		seen := map[string]bool{}
		for _, m := range c.Mounts {
			if m.Type == "volume" && !seen[m.Name] {
				seen[m.Name] = true
				s.Volumes += vols[m.Name]
			}
		}
		out[c.ID] = s
	}
	return out
}

// RefreshSizes measures every container's disk use, unless the last
// measurement is younger than maxAge, and reports whether it measured.
func (d *SocketDocker) RefreshSizes(ctx context.Context, maxAge time.Duration) (bool, error) {
	if d.sizes.fresh(maxAge, time.Now()) {
		return false, nil
	}
	var r dfReport
	// type= limits the work to containers and volumes (Docker 23+; older
	// Engines ignore it and measure images too).
	if err := d.getJSON(ctx, "/system/df", url.Values{"type": {"container", "volume"}}, &r); err != nil {
		return false, err
	}
	now := time.Now()
	d.sizes.set(dfSizes(r, now), now)
	return true, nil
}
