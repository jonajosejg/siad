package proto

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"sync"

	"gitlab.com/NebulousLabs/Sia/modules"

	"gitlab.com/NebulousLabs/errors"
)

var (
	// ErrInvalidHeaderData is returned when we try to deserialize the header from
	// a []byte with incorrect data
	ErrInvalidHeaderData = errors.New("invalid header data")

	// ErrInvalidSectorNumber is returned when the requested sector doesnt' exist
	ErrInvalidSectorNumber = errors.New("invalid sector given - it does not exist")

	// ErrInvalidVersion is returned when the version of the file we are trying to
	// read does not match the current RefCounterHeaderSize
	ErrInvalidVersion = errors.New("invalid file version")

	// RefCounterVersion defines the latest version of the RefCounter
	RefCounterVersion = [8]byte{1}
)

const (
	// RefCounterHeaderSize is the size of the header in bytes
	RefCounterHeaderSize = 8
)

type (
	// RefCounter keeps track of how many references to each sector exist.
	//
	// Once the number of references drops to zero we consider the sector as
	// garbage. We move the sector to end of the data and set the
	// GarbageCollectionOffset to point to it. We can either reuse it to store new
	// data or drop it from the contract at the end of the current period and
	// before the contract renewal.
	RefCounter struct {
		RefCounterHeader

		filepath string // where the refcounter is persisted on disk
		mu       sync.Mutex
	}

	// RefCounterHeader contains metadata about the reference counter file
	RefCounterHeader struct {
		Version [8]byte
	}
)

// LoadRefCounter loads a refcounter from disk
func LoadRefCounter(path string) (RefCounter, error) {
	f, err := os.Open(path)
	if err != nil {
		return RefCounter{}, err
	}
	defer f.Close()

	var header RefCounterHeader
	headerBytes := make([]byte, RefCounterHeaderSize)
	if _, err = f.ReadAt(headerBytes, 0); err != nil {
		return RefCounter{}, errors.AddContext(err, "unable to read from file")
	}
	if err = deserializeHeader(headerBytes, &header); err != nil {
		return RefCounter{}, errors.AddContext(err, "unable to load refcounter header")
	}
	if header.Version != RefCounterVersion {
		return RefCounter{}, errors.AddContext(ErrInvalidVersion, fmt.Sprintf("expected version %d, got version %d", RefCounterVersion, header.Version))
	}
	return RefCounter{
		RefCounterHeader: header,
		filepath:         path,
	}, nil
}

// NewRefCounter creates a new sector reference counter file to accompany a contract file
func NewRefCounter(path string, numSectors uint64) (RefCounter, error) {
	f, err := os.Create(path)
	if err != nil {
		return RefCounter{}, errors.AddContext(err, "Failed to create a file on disk")
	}
	defer f.Close()
	h := RefCounterHeader{
		Version: RefCounterVersion,
	}

	if _, err := f.WriteAt(serializeHeader(h), 0); err != nil {
		return RefCounter{}, err
	}

	if _, err = f.Seek(RefCounterHeaderSize, io.SeekStart); err != nil {
		return RefCounter{}, err
	}
	for i := uint64(0); i < numSectors; i++ {
		if err = binary.Write(f, binary.LittleEndian, uint16(1)); err != nil {
			return RefCounter{}, errors.AddContext(err, "failed to initialize file on disk")
		}
	}
	if err := f.Sync(); err != nil {
		return RefCounter{}, err
	}
	return RefCounter{
		RefCounterHeader: h,
		filepath:         path,
	}, nil
}

// Count returns the number of references to the given sector
func (rc *RefCounter) Count(secNum uint64) (uint16, error) {
	return rc.readCount(secNum)
}

// DecrementCount decrements the reference counter of a given sector. The sector
// is specified by its sequential number (`secNum`).
// Returns the updated number of references or an error.
func (rc *RefCounter) DecrementCount(secNum uint64) (uint16, error) {
	count, err := rc.readCount(secNum)
	if err != nil {
		return 0, errors.AddContext(err, "failed to read count")
	}
	if count == 0 {
		return 0, errors.New("sector count underflow")
	}
	count--
	return count, rc.writeCount(secNum, count)
}

// DeleteRefCounter deletes the counter's file from disk
func (rc *RefCounter) DeleteRefCounter() (err error) {
	return os.Remove(rc.filepath)
}

// IncrementCount increments the reference counter of a given sector. The sector
// is specified by its sequential number (`secNum`).
// Returns the updated number of references or an error.
func (rc *RefCounter) IncrementCount(secNum uint64) (uint16, error) {
	count, err := rc.readCount(secNum)
	if err != nil {
		return 0, errors.AddContext(err, "failed to read count")
	}
	if count == math.MaxUint16 {
		return 0, errors.New("sector count overflow")
	}
	count++
	return count, rc.writeCount(secNum, count)
}

// callSwap swaps the two sectors at the given indices
func (rc *RefCounter) callSwap(i, j uint64) error {
	return rc.managedSwap(i, j)
}

// callTruncate removes the last `n` sector counts from the refcounter file
func (rc *RefCounter) callTruncate(n uint64) error {
	return rc.managedTruncate(n)
}

// managedSwap swaps the counts of the two sectors
func (rc *RefCounter) managedSwap(firstSector, secondSector uint64) error {
	f, err := os.OpenFile(rc.filepath, os.O_RDWR, modules.DefaultFilePerm)
	if err != nil {
		return err
	}
	defer f.Close()

	rc.mu.Lock()
	defer rc.mu.Unlock()
	// swap the values on disk
	firstOffset := int64(offset(firstSector))
	secondOffset := int64(offset(secondSector))
	firstCount := make([]byte, 2)
	secondCount := make([]byte, 2)
	if _, err = f.ReadAt(firstCount, firstOffset); err != nil {
		return err
	}
	if _, err = f.ReadAt(secondCount, secondOffset); err != nil {
		return err
	}
	if _, err = f.WriteAt(firstCount, secondOffset); err != nil {
		return err
	}
	if _, err = f.WriteAt(secondCount, firstOffset); err != nil {
		return err
	}
	return f.Sync()
}

// managedTruncate removes the last `n` sector counts from the refcounter file
func (rc *RefCounter) managedTruncate(n uint64) error {
	fi, err := os.Stat(rc.filepath)
	if err != nil {
		return err
	}
	if n > (uint64(fi.Size())-RefCounterHeaderSize)/2 {
		return fmt.Errorf("cannot truncate more than the total number of counts. number of sectors: %d, sectors to truncate: %d", (uint64(fi.Size())-RefCounterHeaderSize)/2, n)
	}

	rc.mu.Lock()
	defer rc.mu.Unlock()
	// truncate the file on disk
	f, err := os.OpenFile(rc.filepath, os.O_RDWR, modules.DefaultFilePerm)
	if err != nil {
		return err
	}
	defer f.Close()

	return f.Truncate(fi.Size() - int64(n*2))
}

// readCount reads the given sector count from disk
func (rc *RefCounter) readCount(secNum uint64) (uint16, error) {
	f, err := os.Open(rc.filepath)
	if err != nil {
		return 0, errors.AddContext(err, "failed to open the refcounter file")
	}
	defer f.Close()

	b := make([]byte, 2)
	_, err = f.ReadAt(b, int64(offset(secNum)))
	if err == io.EOF {
		return 0, ErrInvalidSectorNumber
	} else if err != nil {
		return 0, errors.AddContext(err, "failed to read from the refcounter file")
	}
	return binary.LittleEndian.Uint16(b), nil
}

// writeCount stores the given sector count on disk
func (rc *RefCounter) writeCount(secNum uint64, c uint16) error {
	f, err := os.OpenFile(rc.filepath, os.O_RDWR, modules.DefaultFilePerm)
	if err != nil {
		return err
	}
	defer f.Close()

	bytes := make([]byte, 2)
	binary.LittleEndian.PutUint16(bytes, c)
	if _, err = f.WriteAt(bytes, int64(offset(secNum))); err != nil {
		return err
	}
	return f.Sync()
}

// deserializeHeader deserializes a header from []byte
func deserializeHeader(b []byte, h *RefCounterHeader) error {
	if uint64(len(b)) < RefCounterHeaderSize {
		return ErrInvalidHeaderData
	}
	copy(h.Version[:], b[:8])
	return nil
}

// offset calculates the byte offset of the sector counter in the file on disk
func offset(secNum uint64) uint64 {
	return RefCounterHeaderSize + secNum*2
}

// serializeHeader serializes a header to []byte
func serializeHeader(h RefCounterHeader) []byte {
	b := make([]byte, RefCounterHeaderSize)
	copy(b[:8], h.Version[:])
	return b
}
