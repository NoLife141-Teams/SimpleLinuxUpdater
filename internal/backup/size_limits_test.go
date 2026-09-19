package backup

import (
	"encoding/base64"
	"testing"
)

func TestBackupUploadLimitFitsAcceptedArchive(t *testing.T) {
	const (
		archiveFramingAllowance = int64(1024 * 1024)
		gcmOverhead             = int64(16)
		envelopeAllowance       = int64(1024*1024 + 4096)
	)
	archiveBytes := MaxExtractedBytes + archiveFramingAllowance
	if MaxArchiveBytes < archiveBytes {
		t.Fatalf("MaxArchiveBytes = %d, need at least %d for accepted payload framing", MaxArchiveBytes, archiveBytes)
	}
	ciphertextBytes := MaxArchiveBytes + gcmOverhead
	if maxCiphertextBytes < ciphertextBytes {
		t.Fatalf("maxCiphertextBytes = %d, need at least %d", maxCiphertextBytes, ciphertextBytes)
	}
	encodedBytes := int64(base64.StdEncoding.EncodedLen(int(ciphertextBytes)))
	if maxEncodedPayloadBytes < encodedBytes {
		t.Fatalf("maxEncodedPayloadBytes = %d, need at least %d", maxEncodedPayloadBytes, encodedBytes)
	}
	requiredUploadBytes := maxEncodedPayloadBytes + envelopeAllowance

	if MaxUploadBytes < requiredUploadBytes {
		t.Fatalf("MaxUploadBytes = %d, need at least %d to encode an accepted archive", MaxUploadBytes, requiredUploadBytes)
	}
}
