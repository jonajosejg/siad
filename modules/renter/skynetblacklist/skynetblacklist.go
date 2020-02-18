package skynetblacklist

import (
	"sync"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/errors"
)

// SkynetBlacklist manages a set of blacklisted skylinks by tracking the
// merkleroots and persists the list to disk
type SkynetBlacklist struct {
	merkleroots      map[crypto.Hash]struct{}
	staticPersistDir string

	mu sync.Mutex
}

// New creates a new SkynetBlacklist
func New(persistDir string) (*SkynetBlacklist, error) {
	sb := &SkynetBlacklist{
		merkleroots:      make(map[crypto.Hash]struct{}),
		staticPersistDir: persistDir,
	}

	// Initialize the persistence of the blacklist
	err := sb.initPersist()
	if err != nil {
		return nil, errors.AddContext(err, "unable to initialize the skynet blacklist persistence")
	}

	return sb, nil
}

// Blacklisted indicates if a skylink is currently blacklisted
func (sb *SkynetBlacklist) Blacklisted(skylink modules.Skylink) bool {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	_, ok := sb.merkleroots[skylink.MerkleRoot()]
	return ok
}

// UpdateSkynetBlacklist updates the list of skylinks that are blacklisted
func (sb *SkynetBlacklist) UpdateSkynetBlacklist(additions, removals []modules.Skylink) error {
	sb.mu.Lock()
	defer sb.mu.Unlock()

	return sb.update(additions, removals)
}
