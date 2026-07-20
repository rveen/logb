package query

import (
	"container/list"
	"sync"

	"github.com/rveen/logb"
)

// batchCache is a bounded LRU of decoded DATA frames, keyed by file offset.
//
// This is where interactivity actually comes from. Panning a chart re-requests
// overlapping windows, so the same frames are asked for again and again; a
// frame that is already decoded costs nothing, while decoding one costs a zstd
// decompress. The cache is bounded by bytes rather than entries because frame
// sizes vary by orders of magnitude between a 7-record event stream and a
// 100k-record CAN trace.
type batchCache struct {
	mu    sync.Mutex
	max   int64
	cur   int64
	ll    *list.List
	items map[uint64]*list.Element

	hits, misses int64
}

type entry struct {
	offset uint64
	batch  *logb.Batch
	size   int64
}

func newBatchCache(maxBytes int64) *batchCache {
	if maxBytes < 1 {
		maxBytes = 1
	}
	return &batchCache{max: maxBytes, ll: list.New(), items: map[uint64]*list.Element{}}
}

func (c *batchCache) get(offset uint64) (*logb.Batch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[offset]
	if !ok {
		c.misses++
		return nil, false
	}
	c.hits++
	c.ll.MoveToFront(el)
	return el.Value.(*entry).batch, true
}

func (c *batchCache) put(offset uint64, b *logb.Batch) {
	// Data is the decompressed record region and dominates a batch's footprint.
	// Tails alias it, so this does not double-count them.
	size := int64(len(b.Data))

	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[offset]; ok {
		c.ll.MoveToFront(el)
		return
	}
	// A single frame larger than the whole budget is not worth evicting
	// everything for; serve it and let it go.
	if size > c.max {
		return
	}
	c.items[offset] = c.ll.PushFront(&entry{offset: offset, batch: b, size: size})
	c.cur += size
	for c.cur > c.max {
		back := c.ll.Back()
		if back == nil {
			break
		}
		e := c.ll.Remove(back).(*entry)
		delete(c.items, e.offset)
		c.cur -= e.size
	}
}

// Stats reports cache effectiveness, for the CLI and for tests that need to
// prove a second request did not re-decode.
func (c *batchCache) stats() (hits, misses int64, bytes int64, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.cur, c.ll.Len()
}
