package levlog

import (
	"github.com/syndtr/goleveldb/leveldb"
	"encoding/binary"
	"sync"
	"github.com/syndtr/goleveldb/leveldb/errors"
)

var (
	NOW_INDEX_B = []byte("NOW_INDEX")
	FIR_INDEX_B = []byte("FIR_INDEX")
)

func Int64ToBytes(i int64) []byte {
	var buf = make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(i))
	return buf
}

func BytesToInt64(buf []byte) int64 {
	return int64(binary.BigEndian.Uint64(buf))
}

type Levlog struct {
	Dbdir      string
	DB         *leveldb.DB
	DbLock     *sync.RWMutex
	FirstIndex int64
	NowIndex   int64
}

func (levlog *Levlog) getNowIndex() (int64, error) {
	var nowIndex int64 = 0
	val, err := levlog.DB.Get(NOW_INDEX_B, nil)
	if err != nil && err != errors.ErrNotFound {
		return 0, err
	} else if err == nil {
		nowIndex = BytesToInt64(val)
	} else {
		nowIndex = 0
		levlog.DB.Put(NOW_INDEX_B, Int64ToBytes(nowIndex), nil)
	}
	return nowIndex, nil
}

