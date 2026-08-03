package backup

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/crypto/scrypt"
)

const (
	maxEnvelopeMetadataBytes = 1024 * 1024
	maxManifestBytes         = 1024 * 1024
)

// TemporaryFile is a bounded, caller-owned file produced by the Backup pipeline.
// Call Remove when the response or restore operation no longer needs it.
type TemporaryFile struct {
	Path string
	Size int64
}

func (f TemporaryFile) Remove() error {
	if strings.TrimSpace(f.Path) == "" {
		return nil
	}
	if err := os.Remove(f.Path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

type ExportFileResult struct {
	File               TemporaryFile
	KnownHostsIncluded bool
}

type ArchiveFileInspection struct {
	Files            map[string]string
	Manifest         Manifest
	Compatible       bool
	MissingResources []string
	dir              string
}

func (i ArchiveFileInspection) Remove() error {
	if strings.TrimSpace(i.dir) == "" {
		return nil
	}
	return os.RemoveAll(i.dir)
}

func createTemporaryFile(dir, pattern string) (*os.File, error) {
	if strings.TrimSpace(dir) == "" {
		dir = os.TempDir()
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		path := file.Name()
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return file, nil
}

func closeTemporaryFile(file *os.File) (TemporaryFile, error) {
	if file == nil {
		return TemporaryFile{}, errors.New("temporary backup file is unavailable")
	}
	path := file.Name()
	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, statErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, closeErr
	}
	return TemporaryFile{Path: path, Size: info.Size()}, nil
}

func copyFileBounded(dst io.Writer, src io.Reader, maxBytes int64) (int64, error) {
	if maxBytes < 0 {
		return 0, errors.New("invalid backup size limit")
	}
	written, err := io.Copy(dst, io.LimitReader(src, maxBytes+1))
	if err != nil {
		return written, err
	}
	if written > maxBytes {
		return written, fmt.Errorf("backup file too large (max %d bytes)", maxBytes)
	}
	return written, nil
}

func copyPathBounded(sourcePath, targetPath string, maxBytes int64) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err := copyFileBounded(target, source, maxBytes); err != nil {
		_ = target.Close()
		return err
	}
	return target.Close()
}

func readPathBounded(path string, maxBytes int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var data bytes.Buffer
	if _, err := copyFileBounded(&data, file, maxBytes); err != nil {
		return nil, err
	}
	return data.Bytes(), nil
}

func (s *Service) CreateDBSnapshotFile() (TemporaryFile, error) {
	tempRoot := s.deps.TempDir()
	tmp, err := createTemporaryFile(tempRoot, "slu-backup-db-*.sqlite")
	if err != nil {
		return TemporaryFile{}, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return TemporaryFile{}, err
	}
	if err := ValidateSnapshotPathInRoot(tmpPath, tempRoot); err != nil {
		_ = os.Remove(tmpPath)
		return TemporaryFile{}, err
	}
	vacuumSQL := "VACUUM INTO " + sqliteStringLiteral(tmpPath)
	if _, err := s.deps.DB().Exec(vacuumSQL); err != nil {
		_ = os.Remove(tmpPath)
		return TemporaryFile{}, fmt.Errorf("snapshot database: %w", err)
	}
	info, err := os.Stat(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return TemporaryFile{}, fmt.Errorf("inspect db snapshot: %w", err)
	}
	if info.Size() > MaxExtractedBytes {
		_ = os.Remove(tmpPath)
		return TemporaryFile{}, fmt.Errorf("database snapshot is too large (max %d bytes)", MaxExtractedBytes)
	}
	return TemporaryFile{Path: tmpPath, Size: info.Size()}, nil
}

func ValidateSnapshotPathInRoot(path, tempRoot string) error {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "" || !filepath.IsAbs(cleanPath) || strings.ContainsAny(cleanPath, "\r\n") {
		return errors.New("invalid backup snapshot path")
	}
	root := filepath.Clean(strings.TrimSpace(tempRoot))
	if root == "" || !filepath.IsAbs(root) {
		return errors.New("invalid backup snapshot path")
	}
	rel, err := filepath.Rel(root, cleanPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("invalid backup snapshot path")
	}
	return nil
}

func hashPath(path string) (ManifestFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return ManifestFile{}, err
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return ManifestFile{}, err
	}
	return ManifestFile{Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}

func (s *Service) buildTarGzFile(paths map[string]string) (TemporaryFile, error) {
	manifest := Manifest{
		Format:    FormatName,
		Version:   FormatVersion,
		CreatedAt: s.deps.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		Files:     make(map[string]ManifestFile, len(paths)),
	}
	var total int64
	for name, path := range paths {
		if !isBackupArchiveEntryName(name) || name == "manifest.json" {
			return TemporaryFile{}, fmt.Errorf("%w: unexpected file %s", ErrMalformed, name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return TemporaryFile{}, err
		}
		if !info.Mode().IsRegular() {
			return TemporaryFile{}, fmt.Errorf("%w: backup source %q must be a regular file", ErrMalformed, name)
		}
		if info.Size() < 0 || info.Size() > MaxExtractedBytes || total+info.Size() > MaxExtractedBytes {
			return TemporaryFile{}, fmt.Errorf("%w: backup payload is too large", ErrMalformed)
		}
		meta, err := hashPath(path)
		if err != nil {
			return TemporaryFile{}, err
		}
		if meta.Size > MaxExtractedBytes || total+meta.Size > MaxExtractedBytes {
			return TemporaryFile{}, fmt.Errorf("%w: backup payload is too large", ErrMalformed)
		}
		total += meta.Size
		manifest.Files[name] = meta
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return TemporaryFile{}, err
	}
	if int64(len(manifestData))+total > MaxExtractedBytes {
		return TemporaryFile{}, fmt.Errorf("%w: backup payload is too large", ErrMalformed)
	}

	out, err := createTemporaryFile(s.deps.TempDir(), "slu-backup-archive-*.tar.gz")
	if err != nil {
		return TemporaryFile{}, err
	}
	path := out.Name()
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(path)
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	writeEntry := func(name string, meta ManifestFile, reader io.Reader) error {
		size := meta.Size
		hdr := &tar.Header{Name: name, Mode: 0600, Size: size, ModTime: s.deps.Now().UTC()}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		hash := sha256.New()
		written, err := io.Copy(tw, io.TeeReader(reader, hash))
		if err != nil {
			return err
		}
		if written != size {
			return fmt.Errorf("backup source %q changed while it was archived", name)
		}
		if meta.SHA256 != "" && !strings.EqualFold(meta.SHA256, hex.EncodeToString(hash.Sum(nil))) {
			return fmt.Errorf("backup source %q changed while it was archived", name)
		}
		return nil
	}
	if err := writeEntry("manifest.json", ManifestFile{Size: int64(len(manifestData))}, bytes.NewReader(manifestData)); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	for _, name := range []string{"servers.db", "config.json", "known_hosts"} {
		path, ok := paths[name]
		if !ok {
			continue
		}
		file, err := os.Open(path)
		if err != nil {
			cleanup()
			return TemporaryFile{}, err
		}
		err = writeEntry(name, manifest.Files[name], file)
		closeErr := file.Close()
		if err != nil {
			cleanup()
			return TemporaryFile{}, err
		}
		if closeErr != nil {
			cleanup()
			return TemporaryFile{}, closeErr
		}
	}
	if err := tw.Close(); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	if err := gz.Close(); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	result, err := closeTemporaryFile(out)
	if err != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, err
	}
	if result.Size > MaxUploadBytes {
		_ = result.Remove()
		return TemporaryFile{}, fmt.Errorf("backup archive is too large (max %d bytes)", MaxUploadBytes)
	}
	return result, nil
}

func validateEnvelopeEncryption(env Envelope) error {
	if env.Format != FormatName || env.Version != FormatVersion {
		return ErrUnsupportedFormat
	}
	if env.KDF.Name != "scrypt" || env.Cipher.Name != "aes-256-gcm" {
		return ErrUnsupportedFormat
	}
	if env.KDF.N != ScryptN || env.KDF.R != ScryptR || env.KDF.P != ScryptP {
		return ErrUnsupportedFormat
	}
	return nil
}

func (s *Service) encryptFile(path, passphrase string) (TemporaryFile, error) {
	plain, err := readPathBounded(path, MaxUploadBytes)
	if err != nil {
		return TemporaryFile{}, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return TemporaryFile{}, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return TemporaryFile{}, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, ScryptN, ScryptR, ScryptP, KeyLen)
	if err != nil {
		return TemporaryFile{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return TemporaryFile{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return TemporaryFile{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	plain = nil

	metadata := struct {
		Format    string     `json:"format"`
		Version   int        `json:"version"`
		CreatedAt string     `json:"created_at"`
		KDF       KDFSpec    `json:"kdf"`
		Cipher    CipherSpec `json:"cipher"`
	}{
		Format:    FormatName,
		Version:   FormatVersion,
		CreatedAt: s.deps.Now().UTC().Format("2006-01-02T15:04:05Z07:00"),
		KDF:       KDFSpec{Name: "scrypt", N: ScryptN, R: ScryptR, P: ScryptP, SaltB64: base64.StdEncoding.EncodeToString(salt)},
		Cipher:    CipherSpec{Name: "aes-256-gcm", NonceB64: base64.StdEncoding.EncodeToString(nonce)},
	}
	metadataJSON, err := json.Marshal(metadata)
	if err != nil {
		return TemporaryFile{}, err
	}
	out, err := createTemporaryFile(s.deps.TempDir(), "slu-backup-encrypted-*"+FileExtension)
	if err != nil {
		return TemporaryFile{}, err
	}
	outPath := out.Name()
	cleanup := func() {
		_ = out.Close()
		_ = os.Remove(outPath)
	}
	if len(metadataJSON) == 0 || metadataJSON[len(metadataJSON)-1] != '}' {
		cleanup()
		return TemporaryFile{}, ErrMalformed
	}
	if _, err := out.Write(metadataJSON[:len(metadataJSON)-1]); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	if _, err := io.WriteString(out, `,"payload_b64":"`); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	encoder := base64.NewEncoder(base64.StdEncoding, out)
	if _, err := encoder.Write(ciphertext); err != nil {
		_ = encoder.Close()
		cleanup()
		return TemporaryFile{}, err
	}
	ciphertext = nil
	if err := encoder.Close(); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	if _, err := io.WriteString(out, `"}`); err != nil {
		cleanup()
		return TemporaryFile{}, err
	}
	result, err := closeTemporaryFile(out)
	if err != nil {
		_ = os.Remove(outPath)
		return TemporaryFile{}, err
	}
	if result.Size > MaxUploadBytes {
		_ = result.Remove()
		return TemporaryFile{}, fmt.Errorf("backup file too large (max %d bytes)", MaxUploadBytes)
	}
	return result, nil
}

func (s *Service) ExportArchiveFile(ctx context.Context, req ExportRequest) (ExportFileResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	req.Passphrase = strings.TrimSpace(req.Passphrase)
	if err := ValidatePassphrase(req.Passphrase); err != nil {
		return ExportFileResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ExportFileResult{}, err
	}

	dbPath := strings.TrimSpace(req.DBSnapshotPath)
	var snapshot TemporaryFile
	if dbPath == "" {
		if req.DBSnapshot != nil {
			tmp, err := createTemporaryFile(s.deps.TempDir(), "slu-backup-db-input-*.sqlite")
			if err != nil {
				return ExportFileResult{}, &ExportError{Stage: ExportStageSnapshot, Err: err}
			}
			if _, err := copyFileBounded(tmp, bytes.NewReader(req.DBSnapshot), MaxExtractedBytes); err != nil {
				path := tmp.Name()
				_ = tmp.Close()
				_ = os.Remove(path)
				return ExportFileResult{}, &ExportError{Stage: ExportStageSnapshot, Err: err}
			}
			snapshot, err = closeTemporaryFile(tmp)
			if err != nil {
				return ExportFileResult{}, &ExportError{Stage: ExportStageSnapshot, Err: err}
			}
		} else {
			var err error
			snapshot, err = s.CreateDBSnapshotFile()
			if err != nil {
				return ExportFileResult{}, &ExportError{Stage: ExportStageSnapshot, Err: err}
			}
		}
		defer snapshot.Remove()
		dbPath = snapshot.Path
	}

	if _, err := os.Stat(s.deps.ConfigPath()); err != nil {
		return ExportFileResult{}, &ExportError{Stage: ExportStageConfig, Err: err}
	}
	paths := map[string]string{"servers.db": dbPath, "config.json": s.deps.ConfigPath()}
	includeKnownHosts := req.IncludeKnownHosts == nil || *req.IncludeKnownHosts
	knownHostsIncluded := false
	if includeKnownHosts {
		if path, exists := KnownHostsBackupPath(s.deps.KnownHostsWritePath, s.deps.DBPath); exists {
			paths["known_hosts"] = path
			knownHostsIncluded = true
		}
	}
	archive, err := s.buildTarGzFile(paths)
	if err != nil {
		return ExportFileResult{}, &ExportError{Stage: ExportStageArchive, Err: err}
	}
	defer archive.Remove()
	if err := ctx.Err(); err != nil {
		return ExportFileResult{}, err
	}
	encrypted, err := s.encryptFile(archive.Path, req.Passphrase)
	if err != nil {
		return ExportFileResult{}, &ExportError{Stage: ExportStageEncrypt, Err: err}
	}
	return ExportFileResult{File: encrypted, KnownHostsIncluded: knownHostsIncluded}, nil
}

func readJSONString(reader *bufio.Reader, writer io.Writer, maxBytes int64) (int64, error) {
	first, err := reader.ReadByte()
	if err != nil || first != '"' {
		return 0, ErrMalformed
	}
	var written int64
	writeBytes := func(data []byte) error {
		if written+int64(len(data)) > maxBytes {
			return ErrMalformed
		}
		if len(data) > 0 && writer != nil {
			if _, err := writer.Write(data); err != nil {
				return err
			}
		}
		written += int64(len(data))
		return nil
	}
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return written, ErrMalformed
		}
		switch b {
		case '"':
			return written, nil
		case '\\':
			escaped, err := reader.ReadByte()
			if err != nil {
				return written, ErrMalformed
			}
			switch escaped {
			case '"', '\\', '/':
				if err := writeBytes([]byte{escaped}); err != nil {
					return written, err
				}
			case 'b':
				if err := writeBytes([]byte{'\b'}); err != nil {
					return written, err
				}
			case 'f':
				if err := writeBytes([]byte{'\f'}); err != nil {
					return written, err
				}
			case 'n':
				if err := writeBytes([]byte{'\n'}); err != nil {
					return written, err
				}
			case 'r':
				if err := writeBytes([]byte{'\r'}); err != nil {
					return written, err
				}
			case 't':
				if err := writeBytes([]byte{'\t'}); err != nil {
					return written, err
				}
			case 'u':
				r, err := readJSONHexRune(reader)
				if err != nil {
					return written, err
				}
				if utf16.IsSurrogate(r) {
					prefix := make([]byte, 2)
					if _, err := io.ReadFull(reader, prefix); err != nil || prefix[0] != '\\' || prefix[1] != 'u' {
						return written, ErrMalformed
					}
					r2, err := readJSONHexRune(reader)
					if err != nil {
						return written, err
					}
					r = utf16.DecodeRune(r, r2)
					if r == utf8.RuneError {
						return written, ErrMalformed
					}
				}
				var encoded [utf8.UTFMax]byte
				n := utf8.EncodeRune(encoded[:], r)
				if err := writeBytes(encoded[:n]); err != nil {
					return written, err
				}
			default:
				return written, ErrMalformed
			}
		default:
			if b < 0x20 {
				return written, ErrMalformed
			}
			if err := writeBytes([]byte{b}); err != nil {
				return written, err
			}
		}
	}
}

func readJSONHexRune(reader io.Reader) (rune, error) {
	var raw [4]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return 0, ErrMalformed
	}
	var value rune
	for _, b := range raw {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value += rune(b - '0')
		case b >= 'a' && b <= 'f':
			value += rune(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value += rune(b-'A') + 10
		default:
			return 0, ErrMalformed
		}
	}
	return value, nil
}

func skipJSONWhitespace(reader *bufio.Reader) error {
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return err
		}
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return reader.UnreadByte()
		}
	}
}

func readRawJSONValue(reader *bufio.Reader, maxBytes int64) ([]byte, error) {
	if err := skipJSONWhitespace(reader); err != nil {
		return nil, ErrMalformed
	}
	var raw bytes.Buffer
	depth := 0
	inString := false
	escaped := false
	for {
		b, err := reader.ReadByte()
		if err != nil {
			return nil, ErrMalformed
		}
		if !inString && depth == 0 && (b == ',' || b == '}') {
			if err := reader.UnreadByte(); err != nil {
				return nil, err
			}
			break
		}
		if int64(raw.Len()+1) > maxBytes {
			return nil, ErrMalformed
		}
		raw.WriteByte(b)
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		switch b {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return nil, ErrMalformed
			}
			depth--
		}
	}
	value := bytes.TrimSpace(raw.Bytes())
	if len(value) == 0 || inString || depth != 0 || !json.Valid(value) {
		return nil, ErrMalformed
	}
	return append([]byte(nil), value...), nil
}

func parseEnvelopePath(path, tempDir string, extractPayload bool) (Envelope, TemporaryFile, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Envelope{}, TemporaryFile{}, err
	}
	if info.Size() <= 0 || info.Size() > MaxUploadBytes {
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	file, err := os.Open(path)
	if err != nil {
		return Envelope{}, TemporaryFile{}, err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	if err := skipJSONWhitespace(reader); err != nil {
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	opening, err := reader.ReadByte()
	if err != nil || opening != '{' {
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	values := make(map[string]json.RawMessage)
	seen := make(map[string]struct{})
	var metadataBytes int64
	var encoded *os.File
	var encodedPath string
	cleanupEncoded := func() {
		if encoded != nil {
			_ = encoded.Close()
		}
		if encodedPath != "" {
			_ = os.Remove(encodedPath)
		}
	}
	defer cleanupEncoded()
	payloadSeen := false
	allowObjectEnd := true
	for {
		if err := skipJSONWhitespace(reader); err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		peek, err := reader.Peek(1)
		if err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		if peek[0] == '}' {
			if !allowObjectEnd {
				return Envelope{}, TemporaryFile{}, ErrMalformed
			}
			_, _ = reader.ReadByte()
			break
		}
		allowObjectEnd = true
		var keyBuffer bytes.Buffer
		if _, err := readJSONString(reader, &keyBuffer, 256); err != nil || !utf8.Valid(keyBuffer.Bytes()) {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		key := keyBuffer.String()
		metadataBytes += int64(len(key))
		if metadataBytes > maxEnvelopeMetadataBytes || len(seen) >= 32 {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		if _, exists := seen[key]; exists {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		seen[key] = struct{}{}
		if err := skipJSONWhitespace(reader); err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		colon, err := reader.ReadByte()
		if err != nil || colon != ':' {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		if err := skipJSONWhitespace(reader); err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		if key == "payload_b64" {
			payloadSeen = true
			var target io.Writer = io.Discard
			if extractPayload {
				encoded, err = createTemporaryFile(tempDir, "slu-backup-base64-*")
				if err != nil {
					return Envelope{}, TemporaryFile{}, err
				}
				encodedPath = encoded.Name()
				target = encoded
			}
			if _, err := readJSONString(reader, target, MaxUploadBytes); err != nil {
				return Envelope{}, TemporaryFile{}, ErrMalformed
			}
		} else {
			value, err := readRawJSONValue(reader, maxEnvelopeMetadataBytes)
			if err != nil {
				return Envelope{}, TemporaryFile{}, err
			}
			metadataBytes += int64(len(value))
			if metadataBytes > maxEnvelopeMetadataBytes {
				return Envelope{}, TemporaryFile{}, ErrMalformed
			}
			switch key {
			case "format", "version", "created_at", "kdf", "cipher":
				values[key] = value
			}
		}
		if err := skipJSONWhitespace(reader); err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		separator, err := reader.ReadByte()
		if err != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		if separator == '}' {
			break
		}
		if separator != ',' {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
		allowObjectEnd = false
	}
	if extractPayload && !payloadSeen {
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	for {
		b, err := reader.ReadByte()
		if err == io.EOF {
			break
		}
		if err != nil || (b != ' ' && b != '\n' && b != '\r' && b != '\t') {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
	}
	var env Envelope
	for key, target := range map[string]any{"format": &env.Format, "version": &env.Version} {
		raw, ok := values[key]
		if !ok || json.Unmarshal(raw, target) != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
	}
	for key, target := range map[string]any{"created_at": &env.CreatedAt, "kdf": &env.KDF, "cipher": &env.Cipher} {
		if raw, ok := values[key]; ok && json.Unmarshal(raw, target) != nil {
			return Envelope{}, TemporaryFile{}, ErrMalformed
		}
	}
	if strings.TrimSpace(env.Format) == "" || env.Version <= 0 {
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	if !extractPayload {
		return env, TemporaryFile{}, nil
	}
	if err := encoded.Close(); err != nil {
		encoded = nil
		return Envelope{}, TemporaryFile{}, err
	}
	encoded = nil
	encodedInput, err := os.Open(encodedPath)
	if err != nil {
		return Envelope{}, TemporaryFile{}, err
	}
	defer encodedInput.Close()
	ciphertext, err := createTemporaryFile(tempDir, "slu-backup-ciphertext-*")
	if err != nil {
		return Envelope{}, TemporaryFile{}, err
	}
	cipherPath := ciphertext.Name()
	decoder := base64.NewDecoder(base64.StdEncoding, encodedInput)
	written, copyErr := copyFileBounded(ciphertext, decoder, MaxUploadBytes)
	if copyErr != nil || written == 0 {
		_ = ciphertext.Close()
		_ = os.Remove(cipherPath)
		return Envelope{}, TemporaryFile{}, ErrMalformed
	}
	artifact, err := closeTemporaryFile(ciphertext)
	if err != nil {
		_ = os.Remove(cipherPath)
		return Envelope{}, TemporaryFile{}, err
	}
	return env, artifact, nil
}

func InspectEnvelopeFile(path string) (Envelope, error) {
	env, _, err := parseEnvelopePath(path, os.TempDir(), false)
	return env, err
}

func (s *Service) decryptFile(path, passphrase string) (TemporaryFile, error) {
	env, ciphertextFile, err := parseEnvelopePath(path, s.deps.TempDir(), true)
	if err != nil {
		return TemporaryFile{}, err
	}
	defer ciphertextFile.Remove()
	if err := validateEnvelopeEncryption(env); err != nil {
		return TemporaryFile{}, err
	}
	salt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env.KDF.SaltB64))
	if err != nil || len(salt) == 0 {
		return TemporaryFile{}, ErrMalformed
	}
	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env.Cipher.NonceB64))
	if err != nil || len(nonce) != 12 {
		return TemporaryFile{}, ErrMalformed
	}
	ciphertext, err := readPathBounded(ciphertextFile.Path, MaxUploadBytes)
	if err != nil {
		return TemporaryFile{}, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, ScryptN, ScryptR, ScryptP, KeyLen)
	if err != nil {
		return TemporaryFile{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return TemporaryFile{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return TemporaryFile{}, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	ciphertext = nil
	if err != nil {
		return TemporaryFile{}, errors.New("invalid passphrase or corrupted backup")
	}
	if int64(len(plain)) > MaxUploadBytes {
		return TemporaryFile{}, ErrMalformed
	}
	out, err := createTemporaryFile(s.deps.TempDir(), "slu-backup-plain-*.tar.gz")
	if err != nil {
		return TemporaryFile{}, err
	}
	outPath := out.Name()
	if _, err := out.Write(plain); err != nil {
		_ = out.Close()
		_ = os.Remove(outPath)
		return TemporaryFile{}, err
	}
	plain = nil
	return closeTemporaryFile(out)
}

func InspectTarGzFileWithLimits(path, tempRoot string, maxFileBytes, maxTotalBytes int64) (ArchiveFileInspection, error) {
	extractDir, err := os.MkdirTemp(tempRoot, "slu-backup-extract-*")
	if err != nil {
		return ArchiveFileInspection{}, err
	}
	if err := os.Chmod(extractDir, 0700); err != nil {
		_ = os.RemoveAll(extractDir)
		return ArchiveFileInspection{}, err
	}
	inspection := ArchiveFileInspection{Files: make(map[string]string), dir: extractDir}
	fail := func(err error) (ArchiveFileInspection, error) {
		_ = inspection.Remove()
		return ArchiveFileInspection{}, err
	}
	archive, err := os.Open(path)
	if err != nil {
		return fail(err)
	}
	defer archive.Close()
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return fail(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := make(map[string]struct{}, 4)
	actual := make(map[string]ManifestFile, 4)
	var total int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(err)
		}
		name, err := validateArchiveEntryHeader(hdr, seen)
		if err != nil {
			return fail(err)
		}
		if hdr.Size < 0 || hdr.Size > maxFileBytes || total+hdr.Size > maxTotalBytes {
			return fail(fmt.Errorf("%w: backup payload is too large", ErrMalformed))
		}
		targetPath := filepath.Join(extractDir, name)
		target, err := os.OpenFile(targetPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fail(err)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(target, hash), io.LimitReader(tr, hdr.Size+1))
		closeErr := target.Close()
		if copyErr != nil {
			return fail(copyErr)
		}
		if closeErr != nil {
			return fail(closeErr)
		}
		if written != hdr.Size {
			return fail(ErrMalformed)
		}
		total += written
		inspection.Files[name] = targetPath
		actual[name] = ManifestFile{Size: written, SHA256: hex.EncodeToString(hash.Sum(nil))}
	}
	manifestPath, ok := inspection.Files["manifest.json"]
	if !ok {
		return fail(ErrMalformed)
	}
	manifestData, err := readPathBounded(manifestPath, maxManifestBytes)
	if err != nil {
		return fail(ErrMalformed)
	}
	var manifest Manifest
	if json.Unmarshal(manifestData, &manifest) != nil || manifest.Files == nil {
		return fail(ErrMalformed)
	}
	missing := make([]string, 0, 2)
	for _, name := range []string{"servers.db", "config.json"} {
		if _, ok := inspection.Files[name]; !ok {
			missing = append(missing, name)
			continue
		}
		if _, ok := manifest.Files[name]; !ok {
			missing = append(missing, name)
		}
	}
	if _, ok := inspection.Files["known_hosts"]; ok {
		if _, declared := manifest.Files["known_hosts"]; !declared {
			return fail(fmt.Errorf("%w: known_hosts is present but not declared", ErrMalformed))
		}
	}
	for name, declared := range manifest.Files {
		if !isBackupArchiveEntryName(name) || name == "manifest.json" {
			return fail(fmt.Errorf("%w: unexpected file %s", ErrMalformed, name))
		}
		observed, exists := actual[name]
		if !exists {
			missing = append(missing, name)
			continue
		}
		if observed.Size != declared.Size {
			return fail(fmt.Errorf("checksum size mismatch for %s", name))
		}
		if !strings.EqualFold(observed.SHA256, declared.SHA256) {
			return fail(fmt.Errorf("checksum mismatch for %s", name))
		}
	}
	sort.Strings(missing)
	inspection.Manifest = manifest
	inspection.Compatible = manifest.Format == FormatName && manifest.Version == FormatVersion
	inspection.MissingResources = compactStrings(missing)
	return inspection, nil
}
