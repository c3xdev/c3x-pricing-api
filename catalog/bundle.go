package catalog

// Bundle assembly: the /catalog endpoint serves every TOML
// definition in one JSON document so a client can fetch the entire
// knowledge base in a single round trip and cache it against the
// content hash.
//
// The server does NOT parse the TOML — definitions travel as raw
// bytes. Parsing/validation is the client engine's job; keeping the
// server agnostic means a catalog-schema evolution never requires a
// server deploy, only new TOML content. schema_version signals
// breaking changes in the TOML schema itself so old clients can
// refuse incompatible bundles instead of mis-pricing.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
)

// SchemaVersion identifies the catalog TOML schema generation. Bump
// only on breaking schema changes (renamed fields, changed
// expression semantics); clients refuse bundles with a major they
// don't understand.
const SchemaVersion = "1"

// Entry is one resource definition: its path identity plus the raw
// TOML source.
type Entry struct {
	Provider string `json:"provider"`
	Kind     string `json:"kind"`
	TOML     string `json:"toml"`
}

// Bundle is the complete knowledge base in transport shape.
type Bundle struct {
	SchemaVersion string  `json:"schema_version"`
	Hash          string  `json:"hash"` // sha256 over sorted entries
	Count         int     `json:"count"`
	Entries       []Entry `json:"entries"`
}

var (
	buildOnce   sync.Once
	cachedJSON  []byte
	cachedHash  string
	cachedCount int
	buildErr    error
)

// BundleJSON returns the serialized bundle, its content hash (used
// as the ETag), and the entry count. Built once per process — the
// embedded FS is immutable.
func BundleJSON() (body []byte, hash string, count int, err error) {
	buildOnce.Do(func() {
		b := Bundle{SchemaVersion: SchemaVersion}
		for _, provider := range []string{"aws", "azure", "gcp"} {
			entries, err := fs.ReadDir(FS, provider)
			if err != nil {
				buildErr = fmt.Errorf("read catalog dir %s: %w", provider, err)
				return
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
					continue
				}
				raw, err := fs.ReadFile(FS, provider+"/"+e.Name())
				if err != nil {
					buildErr = fmt.Errorf("read %s/%s: %w", provider, e.Name(), err)
					return
				}
				b.Entries = append(b.Entries, Entry{
					Provider: provider,
					Kind:     strings.TrimSuffix(e.Name(), ".toml"),
					TOML:     string(raw),
				})
			}
		}
		sort.Slice(b.Entries, func(i, j int) bool {
			if b.Entries[i].Provider != b.Entries[j].Provider {
				return b.Entries[i].Provider < b.Entries[j].Provider
			}
			return b.Entries[i].Kind < b.Entries[j].Kind
		})
		b.Count = len(b.Entries)

		h := sha256.New()
		for _, e := range b.Entries {
			h.Write([]byte(e.Provider))
			h.Write([]byte{0})
			h.Write([]byte(e.Kind))
			h.Write([]byte{0})
			h.Write([]byte(e.TOML))
			h.Write([]byte{0})
		}
		b.Hash = hex.EncodeToString(h.Sum(nil))

		out, err := json.Marshal(b)
		if err != nil {
			buildErr = err
			return
		}
		cachedJSON, cachedHash, cachedCount = out, b.Hash, b.Count
	})
	return cachedJSON, cachedHash, cachedCount, buildErr
}
