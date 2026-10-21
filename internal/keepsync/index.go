package keepsync

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

type liveIndex struct {
	db      *bbolt.DB
	touches map[string]fileEntry
}

func (i *liveIndex) each(visit func(string, fileEntry) error) error {
	return i.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(filesBucket).ForEach(func(key, raw []byte) error {
			rel := string(key)
			if !validRelativeFile(rel) {
				return errors.New("invalid path in persisted Live index")
			}
			var entry fileEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				return err
			}
			return visit(rel, entry)
		})
	})
}

var filesBucket = []byte("files")
var metaBucket = []byte("meta")

func openIndex(path string) (*liveIndex, error) {
	db, err := bbolt.Open(path, 0600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(filesBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(metaBucket)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &liveIndex{db: db, touches: make(map[string]fileEntry)}, nil
}
func (i *liveIndex) close() error { return errors.Join(i.flush(), i.db.Close()) }
func (i *liveIndex) generation() (uint64, error) {
	var gen uint64
	err := i.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(metaBucket)
		value := b.Get([]byte("generation"))
		if len(value) == 8 {
			gen = binary.BigEndian.Uint64(value)
		}
		gen++
		var raw [8]byte
		binary.BigEndian.PutUint64(raw[:], gen)
		return b.Put([]byte("generation"), raw[:])
	})
	return gen, err
}
func (i *liveIndex) get(rel string) (fileEntry, bool, error) {
	var entry fileEntry
	exists := false
	err := i.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(filesBucket).Get([]byte(rel))
		if raw == nil {
			return nil
		}
		exists = true
		return json.Unmarshal(raw, &entry)
	})
	return entry, exists, err
}
func (i *liveIndex) put(rel string, entry fileEntry) error {
	if !validRelativeFile(rel) {
		return errors.New("invalid Live index path")
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	delete(i.touches, rel)
	return i.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(filesBucket).Put([]byte(rel), raw) })
}
func (i *liveIndex) touch(rel string, entry fileEntry) error {
	i.touches[rel] = entry
	if len(i.touches) >= 128 {
		return i.flush()
	}
	return nil
}
func (i *liveIndex) flush() error {
	if len(i.touches) == 0 {
		return nil
	}
	err := i.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(filesBucket)
		for rel, entry := range i.touches {
			raw, err := json.Marshal(entry)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(rel), raw); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		clear(i.touches)
	}
	return err
}
func (i *liveIndex) remove(rel string) error {
	delete(i.touches, rel)
	return i.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(filesBucket).Delete([]byte(rel)) })
}

// Cursor one stale key at a time. No full-directory filename map is retained.
func (i *liveIndex) stale(scope, after string, gen uint64) (string, fileEntry, error) {
	var rel string
	var entry fileEntry
	err := i.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(filesBucket).Cursor()
		var k, v []byte
		if after != "" {
			k, v = c.Seek([]byte(after))
			if string(k) == after {
				k, v = c.Next()
			}
		} else if scope != "" {
			k, v = c.Seek([]byte(scope))
		} else {
			k, v = c.First()
		}
		for k != nil {
			name := string(k)
			if scope != "" && name != scope && !strings.HasPrefix(name, scope+"/") {
				if name < scope+"/" {
					k, v = c.Seek([]byte(scope + "/"))
					continue
				}
				return nil
			}
			if !validRelativeFile(name) {
				return errors.New("invalid path in persisted Live index")
			}
			var candidate fileEntry
			if err := json.Unmarshal(v, &candidate); err != nil {
				return err
			}
			if candidate.Generation != gen && !candidate.Deleted {
				rel, entry = name, candidate
				return nil
			}
			k, v = c.Next()
		}
		return nil
	})
	return rel, entry, err
}
