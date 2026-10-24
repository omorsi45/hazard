package mine

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type rawTempFile interface {
	Write([]byte) (int, error)
	Sync() error
	Close() error
	Name() string
}

func storeRawBlob(rawDir string, raw []byte) (contentSHA256, rawPath string, err error) {
	return storeRawBlobWith(rawDir, raw,
		func(dir, pattern string) (rawTempFile, error) { return os.CreateTemp(dir, pattern) }, os.Link)
}

func storeRawBlobWith(rawDir string, raw []byte, createTemp func(string, string) (rawTempFile, error), publish func(string, string) error) (contentSHA256, rawPath string, err error) {
	if rawDir == "" {
		return "", "", errors.New("raw directory is empty")
	}
	if len(raw) > maxArtifactInputBytes {
		return "", "", errors.New("raw content exceeds artifact size limit")
	}
	dir, err := filepath.Abs(rawDir)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])
	path := filepath.Join(dir, digest)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}
	if err := verifyRawBlob(path, digest, len(raw)); err == nil {
		return digest, path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	temp, err := createTemp(dir, ".raw-*")
	if err != nil {
		return "", "", err
	}
	closed := false
	defer func() {
		if !closed {
			err = errors.Join(err, temp.Close())
		}
		removeErr := os.Remove(temp.Name())
		err = errors.Join(err, removeErr)
		if err != nil {
			contentSHA256, rawPath = "", ""
		}
	}()
	n, writeErr := temp.Write(raw)
	if writeErr != nil {
		return "", "", writeErr
	}
	if n != len(raw) {
		return "", "", io.ErrShortWrite
	}
	if syncErr := temp.Sync(); syncErr != nil {
		return "", "", syncErr
	}
	closed = true
	if closeErr := temp.Close(); closeErr != nil {
		return "", "", closeErr
	}
	if linkErr := publish(temp.Name(), path); linkErr != nil {
		if !errors.Is(linkErr, os.ErrExist) {
			return "", "", linkErr
		}
		if verifyErr := verifyRawBlob(path, digest, len(raw)); verifyErr != nil {
			return "", "", verifyErr
		}
	}
	return digest, path, nil
}

func verifyRawBlob(path, digest string, size int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("raw blob is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	verifyErr := verifyRawContent(file, digest, size)
	return errors.Join(verifyErr, file.Close())
}

func verifyRawContent(reader io.Reader, digest string, size int) error {
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(reader, int64(size)+1))
	if err != nil {
		return err
	}
	if n != int64(size) {
		return fmt.Errorf("raw blob size differs: got %d, want %d", n, size)
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("raw blob content differs from SHA256 filename")
	}
	return nil
}
