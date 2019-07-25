package badger

import (
	"bytes"
	"crypto/aes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dgraph-io/badger/pb"
	"github.com/dgraph-io/badger/y"
)

const (
	// KeyRegistryFileName is the file name for the key registry file.
	KeyRegistryFileName = "KEYREGISTRY"
	// KeyRegistryRewriteFileName is the file name for the rewrite key registry file.
	KeyRegistryRewriteFileName = "REWRITE-KEYREGISTRY"
	// RotationPeriod is the key rotation period for datakey.
	RotationPeriod = 10
)

// SanityText is used to check whether the given user provided storage key is valid or not
var sanityText = []byte("!Badger!Registry!")

// KeyRegistry used to maintain all the data keys.
type KeyRegistry struct {
	sync.RWMutex
	dataKeys    map[uint64]*pb.DataKey
	lastCreated int64
	nextKeyID   uint64
	storageKey  []byte
	fp          *os.File
}

func newKeyRegistry(storageKey []byte) *KeyRegistry {
	return &KeyRegistry{
		dataKeys:   make(map[uint64]*pb.DataKey),
		nextKeyID:  0,
		storageKey: storageKey,
	}
}

// OpenKeyRegistry opens key registry if it exists, otherwise it'll create key registry
// and returns key registry.
func OpenKeyRegistry(dir string, readOnly bool, storageKey []byte) (*KeyRegistry, error) {
	path := filepath.Join(dir, KeyRegistryFileName)
	var flags uint32
	if readOnly {
		flags |= y.ReadOnly
	}
	fp, err := y.OpenExistingFile(path, flags)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		kr := newKeyRegistry(storageKey)
		if readOnly {
			return kr, nil
		}
		if err := RewriteRegistry(dir, kr, storageKey); err != nil {
			return nil, err
		}
		fp, err = y.OpenExistingFile(path, flags)
		if err != nil {
			return nil, err
		}
	}
	kr, err := buildKeyRegistry(fp, storageKey)
	if err != nil {
		fp.Close()
		return nil, err
	}
	return kr, nil
}

func buildKeyRegistry(fp *os.File, storageKey []byte) (*KeyRegistry, error) {
	readPos := int64(0)
	iv, err := y.ReadAt(fp, readPos, aes.BlockSize)
	if err != nil {
		return nil, err
	}
	readPos += aes.BlockSize
	eSanityText, err := y.ReadAt(fp, readPos, len(sanityText))
	if err != nil {
		return nil, err
	}
	if len(storageKey) > 0 {
		var err error
		eSanityText, err = y.XORBlock(storageKey, iv, eSanityText)
		if err != nil {
			return nil, err
		}
	}
	if !bytes.Equal(eSanityText, sanityText) {
		return nil, ErrEncryptionKeyMismatch
	}
	readPos += int64(len(sanityText))
	stat, err := fp.Stat()
	if err != nil {
		return nil, err
	}
	kr := newKeyRegistry(storageKey)
	for {
		if readPos == stat.Size() {
			break
		}
		lenCrcBuf, err := y.ReadAt(fp, readPos, 8)
		if err != nil {
			return nil, err
		}
		readPos += 8
		l := int64(binary.BigEndian.Uint32(lenCrcBuf[0:4]))
		data, err := y.ReadAt(fp, readPos, int(l))
		if err != nil {
			return nil, err
		}
		if crc32.Checksum(data, y.CastagnoliCrcTable) != binary.BigEndian.Uint32(lenCrcBuf[4:]) {
			return nil, errBadChecksum
		}
		dataKey := &pb.DataKey{}
		err = dataKey.Unmarshal(data)
		if err != nil {
			return nil, err
		}
		if len(storageKey) > 0 {
			var err error
			dataKey.Data, err = y.XORBlock(storageKey, dataKey.IV, dataKey.Data)
			if err != nil {
				return nil, err
			}
		}
		if dataKey.KeyID > kr.nextKeyID {
			kr.nextKeyID = dataKey.KeyID
		}
		if dataKey.CreatedAt > (kr.lastCreated) {
			kr.lastCreated = dataKey.CreatedAt
		}
		kr.dataKeys[kr.nextKeyID] = dataKey
		readPos += l
	}
	_, err = fp.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	kr.fp = fp
	return kr, nil
}

// RewriteRegistry will rewrite the existing key registry file with new one
func RewriteRegistry(dir string, reg *KeyRegistry, storageKey []byte) error {
	reWritePath := filepath.Join(dir, KeyRegistryRewriteFileName)
	fp, err := y.OpenTruncFile(reWritePath, false)
	if err != nil {
		return err
	}
	iv, err := y.GenereateIV()
	if err != nil {
		return err
	}
	eSanity := sanityText
	if len(storageKey) > 0 {
		var err error
		eSanity, err = y.XORBlock(storageKey, iv, eSanity)
		if err != nil {
			return err
		}
	}
	if _, err = fp.Write(iv); err != nil {
		fp.Close()
		return err
	}
	if _, err = fp.Write(eSanity); err != nil {
		fp.Close()
		return err
	}
	for _, k := range reg.dataKeys {
		err := storeDataKey(fp, storageKey, k, false)
		if err != nil {
			return err
		}
	}
	if err = y.FileSync(fp); err != nil {
		fp.Close()
		return err
	}
	registryPath := filepath.Join(dir, KeyRegistryFileName)
	if err = fp.Close(); err != nil {
		return err
	}
	if err = os.Rename(reWritePath, registryPath); err != nil {
		return err
	}
	if err = syncDir(dir); err != nil {
		return err
	}
	return nil
}

func (kr *KeyRegistry) dataKey(id uint64) (*pb.DataKey, error) {
	if id == 0 {
		return nil, nil
	}
	dk, ok := kr.dataKeys[id]
	if !ok {
		return nil, ErrInvalidDataKeyID
	}
	return dk, nil
}

func (kr *KeyRegistry) getDataKey() (*pb.DataKey, error) {
	if len(kr.storageKey) == 0 {
		return nil, nil
	}
	diff := time.Since(time.Unix(kr.lastCreated, 0))
	if diff.Hours()/24 < RotationPeriod {
		kr.RLock()
		defer kr.RUnlock()
		dk := kr.dataKeys[kr.nextKeyID]
		return dk, nil
	}
	kr.nextKeyID++
	k := make([]byte, len(kr.storageKey))
	iv, err := y.GenereateIV()
	if err != nil {
		return nil, err
	}
	_, err = rand.Read(k)
	if err != nil {
		return nil, err
	}
	dk := &pb.DataKey{
		KeyID:     kr.nextKeyID,
		Data:      k,
		CreatedAt: time.Now().Unix(),
		IV:        iv,
	}
	err = storeDataKey(kr.fp, kr.storageKey, dk, true)
	if err != nil {
		return nil, err
	}
	// storeDatakey encrypts the datakey So, placing unencrypted key in the memory
	dk.Data = k
	kr.Lock()
	defer kr.Unlock()
	kr.lastCreated = dk.CreatedAt
	kr.dataKeys[kr.nextKeyID] = dk
	return dk, nil
}

// Close closes the key registry.
func (kr *KeyRegistry) Close() error {
	return kr.fp.Close()
}

func storeDataKey(fp *os.File, storageKey []byte, k *pb.DataKey, sync bool) error {
	if len(storageKey) > 0 {
		var err error
		// In memory, we'll have decrypted key.
		k.Data, err = y.XORBlock(storageKey, k.IV, k.Data)
		if err != nil {
			return err
		}
	}
	data, err := k.Marshal()
	if err != nil {
		return err
	}
	var lenCrcBuf [8]byte
	binary.BigEndian.PutUint32(lenCrcBuf[0:4], uint32(len(data)))
	binary.BigEndian.PutUint32(lenCrcBuf[4:8], crc32.Checksum(data, y.CastagnoliCrcTable))
	_, err = fp.Write(lenCrcBuf[:])
	if err != nil {
		return err
	}
	_, err = fp.Write(data)
	if err != nil {
		return err
	}
	if sync {
		err := y.FileSync(fp)
		if err != nil {
			return err
		}
	}
	return nil
}
