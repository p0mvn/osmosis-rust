package cachekv

import (
	"bytes"
	"io"
	"reflect"
	"sort"
	"sync"
	"time"
	"unsafe"

	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/internal/conv"
	"github.com/cosmos/cosmos-sdk/store/listenkv"
	"github.com/cosmos/cosmos-sdk/store/tracekv"
	"github.com/cosmos/cosmos-sdk/store/types"
	"github.com/cosmos/cosmos-sdk/telemetry"
	"github.com/cosmos/cosmos-sdk/types/kv"
)

// If value is nil but deleted is false, it means the parent doesn't have the
// key.  (No need to delete upon Write())
type cValue struct {
	value []byte
	dirty bool
}

// Store wraps an in-memory cache around an underlying types.KVStore.
type Store struct {
	mtx           sync.Mutex
	cache         map[string]*cValue
	deleted       map[string]struct{}
	unsortedCache map[string]struct{}
	sortedCache   *dbm.MemDB // always ascending sorted
	parent        types.KVStore
}

var _ types.CacheKVStore = (*Store)(nil)

// NewStore creates a new Store object
func NewStore(parent types.KVStore) *Store {
	return &Store{
		cache:         make(map[string]*cValue),
		deleted:       make(map[string]struct{}),
		unsortedCache: make(map[string]struct{}),
		sortedCache:   dbm.NewMemDB(),
		parent:        parent,
	}
}

// GetStoreType implements Store.
func (store *Store) GetStoreType() types.StoreType {
	return store.parent.GetStoreType()
}

// Get implements types.KVStore.
func (store *Store) Get(key []byte) (value []byte) {
	store.mtx.Lock()
	defer store.mtx.Unlock()

	types.AssertValidKey(key)

	cacheValue, ok := store.cache[conv.UnsafeBytesToStr(key)]
	if !ok {
		value = store.parent.Get(key)
		store.setCacheValue(key, value, false, false)
	} else {
		value = cacheValue.value
	}

	return value
}

// Set implements types.KVStore.
func (store *Store) Set(key []byte, value []byte) {
	store.mtx.Lock()
	defer store.mtx.Unlock()

	types.AssertValidKey(key)
	types.AssertValidValue(value)

	store.setCacheValue(key, value, false, true)
}

// Has implements types.KVStore.
func (store *Store) Has(key []byte) bool {
	value := store.Get(key)
	return value != nil
}

// Delete implements types.KVStore.
func (store *Store) Delete(key []byte) {
	store.mtx.Lock()
	defer store.mtx.Unlock()
	defer telemetry.MeasureSince(time.Now(), "store", "cachekv", "delete")

	types.AssertValidKey(key)
	store.setCacheValue(key, nil, true, true)
}

// Implements Cachetypes.KVStore.
func (store *Store) Write() {
	store.mtx.Lock()
	defer store.mtx.Unlock()
	defer telemetry.MeasureSince(time.Now(), "store", "cachekv", "write")

	// We need a copy of all of the keys.
	// Not the best, but probably not a bottleneck depending.
	keys := make([]string, 0, len(store.cache))

	for key, dbValue := range store.cache {
		if dbValue.dirty {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	// TODO: Consider allowing usage of Batch, which would allow the write to
	// at least happen atomically.
	for _, key := range keys {
		if store.isDeleted(key) {
			// We use []byte(key) instead of conv.UnsafeStrToBytes because we cannot
			// be sure if the underlying store might do a save with the byteslice or
			// not. Once we get confirmation that .Delete is guaranteed not to
			// save the byteslice, then we can assume only a read-only copy is sufficient.
			store.parent.Delete([]byte(key))
			continue
		}

		cacheValue := store.cache[key]
		if cacheValue.value != nil {
			// It already exists in the parent, hence delete it.
			store.parent.Set([]byte(key), cacheValue.value)
		}
	}

	// Clear the cache using the map clearing idiom
	// and not allocating fresh objects.
	// Please see https://bencher.orijtech.com/perfclinic/mapclearing/
	for key := range store.cache {
		delete(store.cache, key)
	}
	for key := range store.deleted {
		delete(store.deleted, key)
	}
	for key := range store.unsortedCache {
		delete(store.unsortedCache, key)
	}
	store.sortedCache = dbm.NewMemDB()
}

// CacheWrap implements CacheWrapper.
func (store *Store) CacheWrap() types.CacheWrap {
	return NewStore(store)
}

// CacheWrapWithTrace implements the CacheWrapper interface.
func (store *Store) CacheWrapWithTrace(w io.Writer, tc types.TraceContext) types.CacheWrap {
	return NewStore(tracekv.NewStore(store, w, tc))
}

// CacheWrapWithListeners implements the CacheWrapper interface.
func (store *Store) CacheWrapWithListeners(storeKey types.StoreKey, listeners []types.WriteListener) types.CacheWrap {
	return NewStore(listenkv.NewStore(store, storeKey, listeners))
}

//----------------------------------------
// Iteration

// Iterator implements types.KVStore.
func (store *Store) Iterator(start, end []byte) types.Iterator {
	return store.iterator(start, end, true)
}

// ReverseIterator implements types.KVStore.
func (store *Store) ReverseIterator(start, end []byte) types.Iterator {
	return store.iterator(start, end, false)
}

func (store *Store) iterator(start, end []byte, ascending bool) types.Iterator {
	store.mtx.Lock()
	defer store.mtx.Unlock()

	var parent, cache types.Iterator

	if ascending {
		parent = store.parent.Iterator(start, end)
	} else {
		parent = store.parent.ReverseIterator(start, end)
	}

	store.dirtyItems(start, end)
	cache = newMemIterator(start, end, store.sortedCache, store.deleted, ascending)

	return newCacheMergeIterator(parent, cache, ascending)
}

func findStartIndex(strL []string, startQ string) int {
	// Modified binary search to find the very first element in >=startQ.
	if len(strL) == 0 {
		return -1
	}

	var left, right, mid int
	right = len(strL) - 1
	for left <= right {
		mid = (left + right) >> 1
		midStr := strL[mid]
		if midStr == startQ {
			// Handle condition where there might be multiple values equal to startQ.
			// We are looking for the very first value < midStL, that i+1 will be the first
			// element >= midStr.
			for i := mid - 1; i >= 0; i-- {
				if strL[i] != midStr {
					return i + 1
				}
			}
			return 0
		}
		if midStr < startQ {
			left = mid + 1
		} else { // midStrL > startQ
			right = mid - 1
		}
	}
	if left >= 0 && left < len(strL) && strL[left] >= startQ {
		return left
	}
	return -1
}

func findEndIndex(strL []string, endQ string) int {
	if len(strL) == 0 {
		return -1
	}

	// Modified binary search to find the very first element <endQ.
	var left, right, mid int
	right = len(strL) - 1
	for left <= right {
		mid = (left + right) >> 1
		midStr := strL[mid]
		if midStr == endQ {
			// Handle condition where there might be multiple values equal to startQ.
			// We are looking for the very first value < midStL, that i+1 will be the first
			// element >= midStr.
			for i := mid - 1; i >= 0; i-- {
				if strL[i] < midStr {
					return i + 1
				}
			}
			return 0
		}
		if midStr < endQ {
			left = mid + 1
		} else { // midStrL > startQ
			right = mid - 1
		}
	}

	// Binary search failed, now let's find a value less than endQ.
	for i := right; i >= 0; i-- {
		if strL[i] < endQ {
			return i
		}
	}

	return -1
}

type sortState int

const (
	stateUnsorted sortState = iota
	stateAlreadySorted
)

// strToByte is meant to make a zero allocation conversion
// from string -> []byte to speed up operations, it is not meant
// to be used generally, but for a specific pattern to check for available
// keys within a domain.
func strToByte(s string) []byte {
	var b []byte
	hdr := (*reflect.SliceHeader)(unsafe.Pointer(&b))
	hdr.Cap = len(s)
	hdr.Len = len(s)
	hdr.Data = (*reflect.StringHeader)(unsafe.Pointer(&s)).Data
	return b
}

// byteSliceToStr is meant to make a zero allocation conversion
// from []byte -> string to speed up operations, it is not meant
// to be used generally, but for a specific pattern to delete keys
// from a map.
func byteSliceToStr(b []byte) string {
	hdr := (*reflect.StringHeader)(unsafe.Pointer(&b))
	return *(*string)(unsafe.Pointer(hdr))
}

// Constructs a slice of dirty items, to use w/ memIterator.
func (store *Store) dirtyItems(start, end []byte) {
	startStr, endStr := conv.UnsafeBytesToStr(start), conv.UnsafeBytesToStr(end)
	if startStr > endStr {
		// Nothing to do here.
		return
	}

	n := len(store.unsortedCache)
	unsorted := make([]*kv.Pair, 0)
	// If the unsortedCache is too big, its costs too much to determine
	// whats in the subset we are concerned about.
	// If you are interleaving iterator calls with writes, this can easily become an
	// O(N^2) overhead.
	// Even without that, too many range checks eventually becomes more expensive
	// than just not having the cache.
	if n >= 256 {
		for key := range store.unsortedCache {
			cacheValue := store.cache[key]
			keyBz := strToByte(key)
			unsorted = append(unsorted, &kv.Pair{Key: keyBz, Value: cacheValue.value})
		}
	} else {
		// else do a linear scan to determine if the unsorted pairs are in the pool.
		for key := range store.unsortedCache {
			keyBz := strToByte(key)
			if dbm.IsKeyInDomain(keyBz, start, end) {
				cacheValue := store.cache[key]
				unsorted = append(unsorted, &kv.Pair{Key: keyBz, Value: cacheValue.value})
			}
		}
	}
	store.clearUnsortedCacheSubset(unsorted)
}

func (store *Store) clearUnsortedCacheSubset(unsorted []*kv.Pair) {
	n := len(store.unsortedCache)
	if len(unsorted) == n { // This pattern allows the Go compiler to emit the map clearing idiom for the entire map.
		for key := range store.unsortedCache {
			delete(store.unsortedCache, key)
		}
		store.unsortedCache = make(map[string]struct{}, 300)
	} else { // Otherwise, normally delete the unsorted keys from the map.
		for _, kv := range unsorted {
			delete(store.unsortedCache, byteSliceToStr(kv.Key))
		}
	}
	sort.Slice(unsorted, func(i, j int) bool {
		return bytes.Compare(unsorted[i].Key, unsorted[j].Key) < 0
	})

	for _, item := range unsorted {
		if item.Value == nil {
			// deleted element, tracked by store.deleted
			// setting arbitrary value
			store.sortedCache.Set(item.Key, []byte{})
			continue
		}
		err := store.sortedCache.Set(item.Key, item.Value)
		if err != nil {
			panic(err)
		}
	}
}

//----------------------------------------
// etc

// Only entrypoint to mutate store.cache.
func (store *Store) setCacheValue(key, value []byte, deleted bool, dirty bool) {
	keyStr := byteSliceToStr(key)
	store.cache[keyStr] = &cValue{
		value: value,
		dirty: dirty,
	}
	if deleted {
		store.deleted[keyStr] = struct{}{}
	} else {
		delete(store.deleted, keyStr)
	}
	if dirty {
		store.unsortedCache[keyStr] = struct{}{}
	}
}

func (store *Store) isDeleted(key string) bool {
	_, ok := store.deleted[key]
	return ok
}