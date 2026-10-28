package audit

// The hint is an optimization, not a limit on the number of metadata fields.
// Keep it independent of caller-controlled sizes and subsequent extra fields.
const maxMetadataCopyHint = 64

func metadataCopyHint(fields int) int {
	if fields < 0 {
		return 0
	}
	if fields > maxMetadataCopyHint {
		return maxMetadataCopyHint
	}
	return fields
}

// CloneMeta makes a writable shallow copy without arithmetic on an unbounded
// field count. All fields survive; the map grows normally beyond the bounded
// initial allocation hint when copying or enriching a larger metadata map.
func CloneMeta(meta map[string]any) map[string]any {
	out := make(map[string]any, metadataCopyHint(len(meta)))
	for key, value := range meta {
		out[key] = value
	}
	return out
}
