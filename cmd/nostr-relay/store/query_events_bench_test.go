package store

import (
	"encoding/binary"
	"path/filepath"
	"slices"
	"testing"

	"fiatjaf.com/nostr"
)

func seedBenchmarkStore(b *testing.B, totalEvents int) (*SQLiteStore, nostr.ID, nostr.ID) {
	b.Helper()
	ctx := b.Context()
	s, err := Open(filepath.Join(b.TempDir(), "bench.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })

	var targetNoteID nostr.ID
	var sampleID nostr.ID
	for i := range totalEvents {
		var id nostr.ID
		binary.BigEndian.PutUint32(id[28:], uint32(i+1))

		var kind nostr.Kind
		var tags nostr.Tags
		switch {
		case i < totalEvents/5:
			kind = 1
			tags = nostr.Tags{}
			if i == totalEvents/10 {
				targetNoteID = id
				sampleID = id
			}
		case i < (totalEvents*3)/5:
			kind = 6
			var ref nostr.ID
			binary.BigEndian.PutUint32(ref[28:], uint32((i%(totalEvents/5))+1))
			tags = nostr.Tags{{"e", ref.Hex()}, {"p", nostr.NUMS.Hex()}}
		default:
			kind = 7
			var ref nostr.ID
			binary.BigEndian.PutUint32(ref[28:], uint32((i%(totalEvents/5))+1))
			tags = nostr.Tags{{"e", ref.Hex()}, {"p", nostr.NUMS.Hex()}}
		}

		ev := nostr.Event{
			ID:        id,
			PubKey:    nostr.NUMS,
			CreatedAt: nostr.Timestamp(1_700_000_000 + i),
			Kind:      kind,
			Tags:      tags,
			Content:   "benchmark event payload",
		}
		if err := s.SaveEvent(ctx, ev); err != nil {
			b.Fatal(err)
		}
	}
	return s, sampleID, targetNoteID
}

func BenchmarkQueryEvents(b *testing.B) {
	s, sampleID, targetNoteID := seedBenchmarkStore(b, 2000)
	ctx := b.Context()

	cases := []struct {
		name   string
		filter nostr.Filter
	}{
		{
			name:   "IDs",
			filter: nostr.Filter{IDs: []nostr.ID{sampleID}},
		},
		{
			name: "TagAuthorsKinds",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{nostr.NUMS},
				Kinds:   []nostr.Kind{6, 7},
				Tags:    nostr.TagMap{"e": {targetNoteID.Hex()}},
			},
		},
		{
			name: "AuthorsKindsLimit",
			filter: nostr.Filter{
				Authors: []nostr.PubKey{nostr.NUMS},
				Kinds:   []nostr.Kind{1, 6, 7},
				Limit:   50,
			},
		},
	}

	for _, tc := range cases {
		b.Run(tc.name+"/Serial", func(b *testing.B) {
			b.ResetTimer()
			for b.Loop() {
				seq, err := s.QueryEvents(ctx, tc.filter)
				if err != nil {
					b.Fatal(err)
				}
				if len(slices.Collect(seq)) == 0 {
					b.Fatal("expected at least 1 matching event")
				}
			}
		})

		b.Run(tc.name+"/Parallel", func(b *testing.B) {
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					seq, err := s.QueryEvents(ctx, tc.filter)
					if err != nil {
						b.Error(err)
						return
					}
					if len(slices.Collect(seq)) == 0 {
						b.Error("expected at least 1 matching event")
						return
					}
				}
			})
		})
	}
}
