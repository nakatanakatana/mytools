package litestreamvfs

import (
	"os"

	"github.com/ncruces/go-sqlite3"
	ncrucesvfs "github.com/ncruces/go-sqlite3/vfs"
)

func requiresTempFile(flags ncrucesvfs.OpenFlag) bool {
	const tempMask = ncrucesvfs.OPEN_TEMP_DB |
		ncrucesvfs.OPEN_TEMP_JOURNAL |
		ncrucesvfs.OPEN_SUBJOURNAL |
		ncrucesvfs.OPEN_SUPER_JOURNAL |
		ncrucesvfs.OPEN_TRANSIENT_DB |
		ncrucesvfs.OPEN_MAIN_JOURNAL
	if flags&tempMask != 0 {
		return true
	}
	return flags&ncrucesvfs.OPEN_DELETEONCLOSE != 0
}

func openTempFile(name string, flags ncrucesvfs.OpenFlag) (ncrucesvfs.File, ncrucesvfs.OpenFlag, error) {
	_ = flags
	var (
		f   *os.File
		err error
	)
	if name == "" {
		f, err = os.CreateTemp("", "litestream-vfs-temp-*")
	} else {
		f, err = os.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	}
	if err != nil {
		return nil, flags, sqlite3.CANTOPEN
	}
	return &tempFile{f: f, deleteOnClose: flags&ncrucesvfs.OPEN_DELETEONCLOSE != 0 || name == ""}, flags, nil
}

type tempFile struct {
	f             *os.File
	deleteOnClose bool
	lock          ncrucesvfs.LockLevel
}

func (tf *tempFile) Close() error {
	name := tf.f.Name()
	err := tf.f.Close()
	if tf.deleteOnClose {
		if rmErr := os.Remove(name); err == nil {
			err = rmErr
		}
	}
	return err
}

func (tf *tempFile) ReadAt(p []byte, off int64) (int, error) {
	return tf.f.ReadAt(p, off)
}

func (tf *tempFile) WriteAt(p []byte, off int64) (int, error) {
	return tf.f.WriteAt(p, off)
}

func (tf *tempFile) Truncate(size int64) error {
	return tf.f.Truncate(size)
}

func (tf *tempFile) Sync(flags ncrucesvfs.SyncFlag) error {
	_ = flags
	return tf.f.Sync()
}

func (tf *tempFile) Size() (int64, error) {
	info, err := tf.f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (tf *tempFile) Lock(lock ncrucesvfs.LockLevel) error {
	tf.lock = lock
	return nil
}

func (tf *tempFile) Unlock(lock ncrucesvfs.LockLevel) error {
	tf.lock = lock
	return nil
}

func (tf *tempFile) CheckReservedLock() (bool, error) {
	return tf.lock >= ncrucesvfs.LOCK_RESERVED, nil
}

func (tf *tempFile) SectorSize() int {
	return 4096
}

func (tf *tempFile) DeviceCharacteristics() ncrucesvfs.DeviceCharacteristic {
	return 0
}
