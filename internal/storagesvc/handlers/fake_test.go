package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"futo-notes-server/internal/storagesvc/adminapi"
	"futo-notes-server/internal/storagesvc/storage"
)

// memStorage is an in-memory storage.Storage with S3's semantics.
type memStorage struct {
	mu      sync.Mutex
	objects map[string]memObject
	now     func() time.Time

	// getErr fails GetObject for the keys it names.
	getErr map[string]error
	// deleteFail makes DeleteObjects report these keys as failed.
	deleteFail map[string]bool
	// listErr fails every ListObjects.
	listErr error
	// discardPuts stores uploads as synthetic objects of their size.
	discardPuts bool
}

// memObject holds its bytes, or only a size when synthetic, in which case
// reads yield zeros without allocating.
type memObject struct {
	data      []byte
	size      int64
	synthetic bool
	modified  time.Time
}

func newMemStorage() *memStorage {
	return &memStorage{objects: map[string]memObject{}, now: time.Now}
}

func (m *memStorage) put(key string, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, size: int64(len(data)), modified: m.now()}
}

func (m *memStorage) putSynthetic(key string, size int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{size: size, synthetic: true, modified: m.now()}
}

func (m *memStorage) putAt(key string, data []byte, modified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, size: int64(len(data)), modified: modified}
}

func (m *memStorage) get(key string) (memObject, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o, ok
}

func (m *memStorage) keys() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (m *memStorage) PutObject(_ context.Context, key string, body io.Reader, size int64) error {
	obj := memObject{size: size, synthetic: m.discardPuts}
	if m.discardPuts {
		n, err := io.Copy(io.Discard, io.LimitReader(body, size))
		if err == nil && n < size {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			return fmt.Errorf("%w: %w", storage.ErrBody, err)
		}
	} else {
		obj.data = make([]byte, size)
		if _, err := io.ReadFull(body, obj.data); err != nil {
			return fmt.Errorf("%w: %w", storage.ErrBody, err)
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objects[key]; ok {
		return storage.ErrExists
	}
	obj.modified = m.now()
	m.objects[key] = obj
	return nil
}

func (m *memStorage) GetObject(_ context.Context, key, byteRange string) (*storage.Object, error) {
	if err := m.getErr[key]; err != nil {
		return nil, err
	}
	o, ok := m.get(key)
	if !ok {
		return nil, storage.ErrNotFound
	}
	if byteRange == "" {
		return &storage.Object{Body: o.reader(0, o.size), Length: o.size}, nil
	}
	first, last, err := resolveRange(byteRange, o.size)
	if err != nil {
		return nil, err
	}
	n := last - first + 1
	return &storage.Object{
		Body:         o.reader(first, n),
		Length:       n,
		ContentRange: fmt.Sprintf("bytes %d-%d/%d", first, last, o.size),
	}, nil
}

func (o memObject) reader(offset, n int64) io.ReadCloser {
	if o.synthetic {
		return io.NopCloser(io.LimitReader(zeros{}, n))
	}
	return io.NopCloser(bytes.NewReader(o.data[offset : offset+n]))
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// resolveRange applies RFC 9110 to the single ranges singleRange lets
// through.
func resolveRange(header string, size int64) (int64, int64, error) {
	spec := strings.TrimPrefix(header, "bytes=")
	first, last, _ := strings.Cut(spec, "-")
	unsatisfiable := &storage.RangeError{Size: size}
	if first == "" {
		n, _ := strconv.ParseInt(last, 10, 64)
		if n == 0 || size == 0 {
			return 0, 0, unsatisfiable
		}
		return max(0, size-n), size - 1, nil
	}
	start, _ := strconv.ParseInt(first, 10, 64)
	if start >= size {
		return 0, 0, unsatisfiable
	}
	end := size - 1
	if last != "" {
		e, _ := strconv.ParseInt(last, 10, 64)
		end = min(e, size-1)
	}
	return start, end, nil
}

func (m *memStorage) ListObjects(_ context.Context, prefix, startAfter string, limit int) (storage.ListPage, error) {
	if m.listErr != nil {
		return storage.ListPage{}, m.listErr
	}
	var page storage.ListPage
	for _, key := range m.keys() {
		if !strings.HasPrefix(key, prefix) || key <= startAfter {
			continue
		}
		if len(page.Objects) == limit {
			page.Truncated = true
			break
		}
		o, _ := m.get(key)
		page.Objects = append(page.Objects, storage.ObjectInfo{Key: key, Size: o.size, LastModified: o.modified})
		page.LastKey = key
	}
	return page, nil
}

func (m *memStorage) DeleteObjects(_ context.Context, keys []string) ([]storage.DeleteError, error) {
	if len(keys) > adminapi.MaxDeleteKeys {
		return nil, errors.New("too many keys")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var failed []storage.DeleteError
	for _, key := range keys {
		if m.deleteFail[key] {
			failed = append(failed, storage.DeleteError{Key: key, Message: "AccessDenied: denied"})
			continue
		}
		delete(m.objects, key)
	}
	return failed, nil
}
