package audit

import (
	"fmt"
	"reflect"
	"testing"
)

func TestMetadataCopyHintBoundsThroughMaximumInteger(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	for _, fields := range []int{-maxInt - 1, -1, 0, 1, 63, 64, 65, 4096, maxInt - 2, maxInt - 1, maxInt} {
		t.Run(fmt.Sprint(fields), func(t *testing.T) {
			got := metadataCopyHint(fields)
			if got < 0 || got > maxMetadataCopyHint || (fields >= 0 && got > fields) {
				t.Fatalf("unsafe allocation hint for %d fields: %d", fields, got)
			}
		})
	}
}

func TestCloneMetaPreservesAllFieldsAcrossAllocationHintBoundary(t *testing.T) {
	for _, count := range []int{0, 63, 64, 65, 4096} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var original map[string]any
			if count != 0 {
				original = make(map[string]any)
				for i := range count {
					original[fmt.Sprintf("field-%d", i)] = i
				}
			}
			copied := CloneMeta(original)
			if len(copied) != count {
				t.Fatalf("copy lost fields: got %d, want %d", len(copied), count)
			}
			for key, value := range original {
				if copied[key] != value {
					t.Fatalf("lost metadata field %s", key)
				}
			}
			copied["account_id"], copied["part"] = int64(7), 2
			if len(original) != count {
				t.Fatal("enrichment modified caller metadata")
			}
		})
	}
}

func TestStableActorIdentityPreservesLargeFieldSets(t *testing.T) {
	meta := map[string]any{"account_id": int64(99), "personal_token_id": "caller-value"}
	for i := range 65 {
		meta[fmt.Sprintf("field-%d", i)] = i
	}
	before := CloneMeta(meta)
	out := withStableActorIdentity("account:7/token:0123456789abcdef0123456789abcdef", meta)
	if !reflect.DeepEqual(meta, before) || len(out) != len(meta) {
		t.Fatal("identity enrichment mutated or dropped original fields")
	}
	if out["account_id"] != int64(7) || out["personal_token_id"] != "0123456789abcdef0123456789abcdef" {
		t.Fatal("authenticated identity did not replace caller identity")
	}
	for i := range 65 {
		key := fmt.Sprintf("field-%d", i)
		if out[key] != i {
			t.Fatalf("identity enrichment lost %s", key)
		}
	}
}
