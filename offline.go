package pwnedpass

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/exp/mmap"
)

const (
	// DatabaseFilename is the default path to the database.
	DatabaseFilename        = "pwned-passwords.bin"
	UpdatedDatabaseFilename = "updated-pwned-passwords.bin"
	LockFileName            = "pwned-passwords.lock"

	// IndexSegmentSize is the exact size of the index segment in bytes.
	IndexSegmentSize = 256 << 16 << 3 // exactly 256^3 MB

	// DataSegmentOffset indicates the byte offset in the database where
	// the data segment begins.
	DataSegmentOffset = IndexSegmentSize

	// caphextable is used to hex-encode a string using capital letters. It's a
	// slight variation to the strategy used by the stdlib's hex.Encode.
	caphextable = "0123456789ABCDEF"
)

var (
	// FirstPrefix is the very first prefix in the dataset. It is intended
	// to be used as a parameter to Scan.
	FirstPrefix = [3]byte{0x00, 0x00, 0x00}

	// LastPrefix is the very last prefix in the dataset. It is intended
	// to be used as a parameter to Scan.
	LastPrefix = [3]byte{0xFF, 0xFF, 0xFF}

	// bufferPool is a pool of large-ish buffer objects available for reuse.
	bufferPool = &sync.Pool{New: func() interface{} {
		return make([]byte, 8<<10)
	}}

	logger *zap.Logger
)

type (
	// An OfflineDatabase is a client for querying Pwned Passwords locally.
	OfflineDatabase struct {
		file          *DatabaseFile
		cron          *cron.Cron
		dbFile        string
		updatedDbFile string
	}

	DatabaseFile struct {
		database readCloserAt
	}

	// readCloserAt is an io.ReaderAt that can be Closed and whose
	// length can be obtained.
	//
	// Note that both *mmap.ReaderAt and *bytes.Reader implement this
	// interface.
	readCloserAt interface {
		io.ReaderAt
		io.Closer
		Len() int
	}
)

// NewOfflineDatabase opens a new OfflineDatabase using the data in the given
// database file.
func NewOfflineDatabase(dbFile string, updatedDbFile string) (*OfflineDatabase, error) {
	lockExists := false
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.TimeKey = "timestamp"
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	config := zap.Config{
		Level:             zap.NewAtomicLevelAt(zap.InfoLevel),
		Development:       false,
		DisableCaller:     true,
		DisableStacktrace: false,
		Sampling:          nil,
		Encoding:          "json",
		EncoderConfig:     encoderCfg,
		OutputPaths: []string{
			"stderr",
		},
		ErrorOutputPaths: []string{
			"stderr",
		},
		InitialFields: map[string]interface{}{
			"pid": os.Getpid(),
		},
	}
	logger = zap.Must(config.Build())
	for {
		if _, err := os.Stat(LockFileName); err == nil {
			lockExists = true
		} else {
			break
		}
		if _, err := os.Stat(dbFile); err == nil {
			break
		} else {
			// Check if error indicates a missing file?
			if os.IsNotExist(err) && lockExists {
				logger.Warn("Lock file exists, but database file does not. Waiting for lock to be released.")
				time.Sleep(1 * time.Minute)
			}
		}
	}

	if _, err := os.Stat(updatedDbFile); err == nil {
		if !lockExists {
			logger.Info("Updating database")
			if err := os.Rename(updatedDbFile, dbFile); err != nil {
				return nil, fmt.Errorf("error moving updated database: %s", err)
			}
			logger.Info("Database updated")
		}
	}

	db, err := mmap.Open(dbFile)
	if err != nil {
		return nil, fmt.Errorf("error opening index: %s", err)
	}
	logger.Info("Database opened")
	c := cron.New()
	odb := &OfflineDatabase{
		file:          &DatabaseFile{db},
		cron:          c,
		dbFile:        dbFile,
		updatedDbFile: updatedDbFile,
	}

	c.AddFunc("@hourly", odb.Reload)
	c.Start()
	return odb, nil

}

func (odb *OfflineDatabase) Reload() {
	if _, err := os.Stat(odb.updatedDbFile); err == nil {
		lockExists := false
		if _, err := os.Stat(LockFileName); err == nil {
			lockExists = true
		}
		if !lockExists {
			logger.Sugar().Infof("Updating database %p from cron job", odb.file)
			odb.file.Close()
			if err := os.Rename(odb.updatedDbFile, odb.dbFile); err != nil {
				logger.Error("error replacing updated database", zap.Error(err))
			}
			db, err := mmap.Open(odb.dbFile)
			if err != nil {
				logger.Error("error loading updated database", zap.Error(err))
			}
			odb.file = &DatabaseFile{db}
			logger.Sugar().Infof("Database updated %p from cron job", odb.file)
		}
	}
}

func (df *DatabaseFile) Close() error {
	logger.Sugar().Infof("Closing database %p", df.database)
	return df.database.Close()
}

// Close frees resources associated with the database.
func (od *OfflineDatabase) Close() error {
	od.cron.Stop()
	return od.file.Close()
}

// Pwned checks how frequently the given hash is included in the Pwned Passwords
// database.
//
// Pwned will only return an error in the case of an invalid database file. Hashes
// that are not found in the database will return a frequency of 0 and a nil error.
func (od *OfflineDatabase) Pwned(hash [20]byte) (frequency int, err error) {

	var prefix [3]byte
	copy(prefix[:], hash[:])

	// look up location in the index
	start, length, err := od.lookup(prefix)
	if err != nil {
		return 0, err
	}

	var (
		// lo and hi are the bounds of the binary search;
		// the range narrows to zero as the search proceeds
		lo, hi int = 0, int(length / 19)

		// test is the current index to test; always between lo and hi
		test int = hi

		// rbuf is a temporary buffer to read into
		rbuf [19]byte

		// suffix holds the last 17 bytes of the hash
		// count holds the binary-encoded frequency of that hash
		suffix, count = rbuf[0:17], rbuf[17:19]
	)

	// binary search between start and start+length
	for {

		// bisect
		test = lo + ((hi - lo) / 2)

		// lookup
		if _, err := od.file.database.ReadAt(rbuf[:], DataSegmentOffset+start+int64(test*19)); err != nil {
			return 0, err
		}

		// compare
		switch bytes.Compare(hash[3:20], suffix) {
		case -1: // hash < curhash
			hi = test
		case 1: // hash > curhash
			lo = test
		case 0: // equal!
			return int(binary.BigEndian.Uint16(count)), nil
		}

		// not found?
		if hi-lo == 1 {
			return 0, nil
		}
	}

}

// Scan iterates through all hashes between startPrefix and endPrefix (inclusive).
// Iteration begins at the first hash with a prefix of startPrefix and continues
// until one of these conditions is met:
//
//  1. the last hash with a prefix of endPrefix has been reached,
//  2. the callback returns "true" to indicate a stop is requested,
//  3. the end of the hash database is reached.
//
// The binary-encoded hash is written into the hash slice argument, which must be
// at least 20 bytes long (providing a smaller slice will result in a panic).
//
// Scan will only return an error in the case of an invalid database file.
func (od *OfflineDatabase) Scan(startPrefix, endPrefix [3]byte, hash []byte, cb func(frequency uint16) bool) error {

	if bytes.Compare(startPrefix[:], endPrefix[:]) == 1 {
		return errors.New("invalid range: startPrefix > endPrefix")
	}

	buffer := bufferPool.Get().([]byte)

	var shortPrefix [3]byte = startPrefix
	var fullPrefix [4]byte
	copy(fullPrefix[1:4], startPrefix[0:3])

	copy(hash[0:3], startPrefix[0:3])

	var currentPrefix uint32 = binary.BigEndian.Uint32(fullPrefix[:])

	for {

		// look up location in the index
		start, length, err := od.lookup(shortPrefix)
		if err != nil {
			return err
		}

		// read from the data file
		if _, err := od.file.database.ReadAt(buffer[0:length], DataSegmentOffset+start); err != nil {
			return err
		}

		// decode a hash+freq pair, and invoke the callback
		for offset := int64(0); offset < length; offset += 19 {

			copy(hash[3:20], buffer[offset:offset+17])
			frequency := uint16(binary.BigEndian.Uint16(buffer[offset+17 : offset+19]))

			if stop := cb(frequency); stop {
				return nil
			}

		}

		// stop if we've reached the end prefix, inclusive
		if shortPrefix == endPrefix {
			break
		}

		// advance the current prefix pointer
		currentPrefix++
		binary.BigEndian.PutUint32(fullPrefix[0:4], currentPrefix)
		copy(shortPrefix[0:3], fullPrefix[1:4])
		copy(hash[0:3], fullPrefix[1:4])

		// stop if we're reaching beyond the end
		if currentPrefix > 256<<16 {
			break
		}

	}

	return nil
}

// lookup returns the location of a block of data in the index.
func (od *OfflineDatabase) lookup(start [3]byte) (location, length int64, err error) {

	// get a small buffer to reuse for various things here
	var buffer [16]byte

	// get location as integer
	copy(buffer[1:4], start[:])
	prefixIndex := binary.BigEndian.Uint32(buffer[0:4]) // number between 0x00000000 and 0x00FFFFFF

	var loc, dataLen int64

	switch start {

	// If we're looking up 0x00FFFFFF there won't be a next one to check, so don't try.
	case [3]byte{0xFF, 0xFF, 0xFF}:

		// read the required index
		if _, err := od.file.database.ReadAt(buffer[0:8], int64(prefixIndex)*8); err != nil {
			return 0, 0, err
		}

		// look up locations and calculate length
		loc = int64(binary.BigEndian.Uint64(buffer[0:8]))
		dataLen = int64(od.file.database.Len()-IndexSegmentSize) - loc

	default:

		// read the required index, and the next one (to calculate length)
		if _, err := od.file.database.ReadAt(buffer[0:16], int64(prefixIndex)*8); err != nil {
			return 0, 0, err
		}

		// look up locations and calculate length
		var nextLoc int64
		loc, nextLoc = int64(binary.BigEndian.Uint64(buffer[0:8])), int64(binary.BigEndian.Uint64(buffer[8:16]))
		dataLen = nextLoc - loc

	}

	return loc, dataLen, nil

}

// ServeHTTP implements the http.Handler interface by approximating the online
// Pwned Password V2 API. The following routes are available:
//
//	/pwnedpassword/password
//	/pwnedpassword/hash
//	/range/ABCDE
//
// Their behavior is very similar to that of the online equivalent; the same
// documentation should apply.
func (od *OfflineDatabase) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	switch {

	case strings.HasPrefix(r.URL.Path, "/pwnedpassword/"):

		// get password
		pw := bytes.TrimPrefix([]byte(r.URL.Path), []byte("/pwnedpassword/"))

		var hash [20]byte
		if hh, ok := isHash(pw); ok {
			// already a hash, just use it
			hash = hh
		} else {
			// not a hash, hash it now
			hash = sha1.Sum([]byte(pw))
		}
		logger.Sugar().Infof("checking password: %s", hex.EncodeToString(hash[:]))
		frequency, err := od.Pwned(hash)
		if err != nil {
			logger.Sugar().Warnf("error checking password: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		if frequency == 0 {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, 0)
		} else {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, frequency)
		}

	case strings.HasPrefix(r.URL.Path, "/range/"):

		// get password
		prefix := bytes.TrimPrefix([]byte(r.URL.Path), []byte("/range/"))

		// validate hash
		if !isHashPrefix(prefix) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("The hash prefix was not in a valid format"))
			return
		}

		var hash [20]byte      // the binary-encoded SHA1 hash
		var hexhash [40]byte   // the hex-encoded SHA1 hash
		var buffer [64]byte    // small buffer for use in formatting response lines
		var start, end [3]byte // scan boundaries

		// calculate the scan boundaries
		hex.Decode(start[:], append(prefix, byte('0')))
		hex.Decode(end[:], append(prefix, byte('F')))

		// perform the scan
		response := bytes.NewBuffer(buffer[:])
		err := od.Scan(start, end, hash[:], func(freq uint16) bool {

			// convert to capital hex bytes
			for i, v := range hash[:] {
				hexhash[i*2] = caphextable[v>>4]
				hexhash[i*2+1] = caphextable[v&0x0f]
			}

			response.Truncate(0)
			response.Write(hexhash[5:])
			response.Write([]byte{':'})
			response.WriteString(strconv.FormatInt(int64(freq), 10))
			response.Write([]byte{'\r', '\n'})
			w.Write(response.Bytes())

			return false

		})
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	default:
		w.WriteHeader(http.StatusNotFound)
		return

	}

}

// isHash indicates whether the given input is already a hash value,
// and returns it if so.
func isHash(s []byte) (hash [20]byte, ok bool) {

	// not a hash if it's not the right length
	if len(s) != 40 {
		ok = false
		return
	}

	// decode the hex bytes
	if _, err := hex.Decode(hash[:], s); err != nil {
		ok = false
		return
	}

	ok = true
	return

}

// isHashPrefix indicates whether s is a valid 5-character hex-encoded
// prefix suitable for use as the parameter to a range request.
func isHashPrefix(s []byte) bool {

	// not a hash prefix if it's not the right length
	if len(s) != 5 {
		// fmt.Printf("not the right length: %d\n", len(s))
		return false
	}

	for _, c := range s {
		isUpper := c >= 'A' && c <= 'Z'
		isLower := c >= 'a' && c <= 'z'
		isNum := c >= '0' && c <= '9'
		if !(isUpper || isLower || isNum) {
			// fmt.Printf("illegal character: %x (%c) (%t %t %t)\n", c, c, isUpper, isLower, isNum)
			return false
		}
	}

	return true

}
