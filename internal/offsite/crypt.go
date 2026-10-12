package offsite

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Encrypted backups: what leaves the server can only be read with the
// passphrase. Stream format (AES-256-GCM in 1 MiB chunks, so files of any size
// stream without temp copies):
//
//	"P8SENC1\n" | salt (16) | nonce prefix (8)
//	then chunks: length (4, big endian) | GCM(chunk) — nonce = prefix | counter (4),
//	additional data = 1 for the last chunk, 0 otherwise (a cut-off file is detected).

const (
	magic     = "P8SENC1\n"
	chunkSize = 1 << 20
	kdfRounds = 200_000
)

var ErrPassphrase = errors.New("wrong passphrase or damaged file")

func deriveKey(pass string, salt []byte) ([]byte, error) {
	return pbkdf2.Key(sha256.New, pass, salt, kdfRounds, 32)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	b, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// Encrypt returns a reader yielding the encrypted form of r.
func Encrypt(r io.Reader, pass string) (io.Reader, error) {
	salt := make([]byte, 16)
	prefix := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	key, err := deriveKey(pass, salt)
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(encryptTo(pw, r, aead, salt, prefix))
	}()
	return pr, nil
}

func encryptTo(w io.Writer, r io.Reader, aead cipher.AEAD, salt, prefix []byte) error {
	if _, err := w.Write(append(append([]byte(magic), salt...), prefix...)); err != nil {
		return err
	}
	buf := make([]byte, chunkSize)
	next := make([]byte, chunkSize)
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return err
	}
	nonce := make([]byte, 12)
	copy(nonce, prefix)
	for counter := uint32(0); ; counter++ {
		// read ahead to know whether this chunk is the last
		m, nerr := 0, error(io.EOF)
		if n == chunkSize {
			m, nerr = io.ReadFull(r, next)
			if nerr != nil && nerr != io.ErrUnexpectedEOF && nerr != io.EOF {
				return nerr
			}
		}
		last := m == 0
		ad := []byte{0}
		if last {
			ad[0] = 1
		}
		binary.BigEndian.PutUint32(nonce[8:], counter)
		ct := aead.Seal(nil, nonce, buf[:n], ad)
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(ct)))
		if _, err := w.Write(l[:]); err != nil {
			return err
		}
		if _, err := w.Write(ct); err != nil {
			return err
		}
		if last {
			return nil
		}
		if counter == ^uint32(0) {
			return errors.New("file too large to encrypt")
		}
		buf, next, n = next, buf, m
	}
}

// Decrypt writes the plain form of an encrypted stream to w.
func Decrypt(w io.Writer, r io.Reader, pass string) error {
	hdr := make([]byte, len(magic)+16+8)
	if _, err := io.ReadFull(r, hdr); err != nil || string(hdr[:len(magic)]) != magic {
		return fmt.Errorf("not an encrypted P8S backup")
	}
	key, err := deriveKey(pass, hdr[len(magic):len(magic)+16])
	if err != nil {
		return err
	}
	aead, err := newGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, 12)
	copy(nonce, hdr[len(magic)+16:])
	buf := make([]byte, chunkSize+aead.Overhead())
	for counter := uint32(0); ; counter++ {
		var l [4]byte
		if _, err := io.ReadFull(r, l[:]); err != nil {
			return fmt.Errorf("%w (file ends early)", ErrPassphrase)
		}
		n := binary.BigEndian.Uint32(l[:])
		if int(n) > len(buf) || int(n) < aead.Overhead() {
			return ErrPassphrase
		}
		if _, err := io.ReadFull(r, buf[:n]); err != nil {
			return fmt.Errorf("%w (file ends early)", ErrPassphrase)
		}
		binary.BigEndian.PutUint32(nonce[8:], counter)
		last := false
		pt, err := aead.Open(nil, nonce, buf[:n], []byte{0})
		if err != nil {
			if pt, err = aead.Open(nil, nonce, buf[:n], []byte{1}); err != nil {
				return ErrPassphrase
			}
			last = true
		}
		if _, err := w.Write(pt); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}
