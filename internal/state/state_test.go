package state

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPlanSegmentsTilesTheFileExactly(t *testing.T) {
	sizes := []int64{1, 2, 7, 63, 64, 65, 1000, 6519558, 10_000_000}
	counts := []int{1, 2, 3, 8, 64, 1000}

	for _, size := range sizes {
		for _, n := range counts {
			segs := PlanSegments(size, n)

			want := n
			if int64(n) > size {
				want = int(size)
			}
			if len(segs) != want {
				t.Fatalf("size %d, n %d: planned %d segments, want %d", size, n, len(segs), want)
			}

			var sum int64
			for i, s := range segs {
				if s.Index != i {
					t.Fatalf("size %d, n %d: segment %d carries index %d", size, n, i, s.Index)
				}
				if s.Len() <= 0 {
					t.Fatalf("size %d, n %d: segment %d has a non-positive length", size, n, i)
				}
				if i == 0 && s.Start != 0 {
					t.Fatalf("size %d, n %d: first segment starts at %d", size, n, s.Start)
				}
				if i > 0 && s.Start != segs[i-1].End+1 {
					t.Fatalf("size %d, n %d: gap or overlap between segments %d and %d", size, n, i-1, i)
				}
				if s.Remaining() != s.Len() {
					t.Fatalf("size %d, n %d: a fresh segment reports %d bytes remaining", size, n, s.Remaining())
				}
				sum += s.Len()
			}
			if sum != size {
				t.Fatalf("size %d, n %d: segments cover %d bytes", size, n, sum)
			}
			if last := segs[len(segs)-1]; last.End != size-1 {
				t.Fatalf("size %d, n %d: last segment ends at %d", size, n, last.End)
			}
		}
	}
}

func TestPlanSegmentsHandlesDegenerateInput(t *testing.T) {
	// A connection count below one must still produce a usable plan.
	segs := PlanSegments(1000, 0)
	if len(segs) != 1 || segs[0].Len() != 1000 {
		t.Fatalf("PlanSegments(1000, 0) = %+v, want one segment covering the file", segs)
	}

	// An unknown size (0) must not produce an empty slice that later code would
	// index into.
	degen := PlanSegments(0, 8)
	if len(degen) != 1 {
		t.Fatalf("PlanSegments(0, 8) returned %d segments, want 1 placeholder", len(degen))
	}
}

func TestStoreRoundTrip(t *testing.T) {
	store := Store{Dir: filepath.Join(t.TempDir(), "downloads")}

	d := &Download{
		ID:        NewID(),
		URL:       "https://example.test/file.bin",
		Dest:      "file.bin",
		PartPath:  "file.bin.part",
		TotalSize: 4096,
		Segmented: true,
		ETag:      `"abc"`,
		Segments:  PlanSegments(4096, 2),
		Status:    StatusQueued,
		CreatedAt: time.Now(),
	}
	if err := store.Save(d); err != nil {
		t.Fatalf("Save: %v", err)
	}
	d.Segments[0].Written = 1024
	d.Segments[0].Done = true
	d.Status = StatusPaused
	if err := store.Save(d); err != nil {
		t.Fatalf("Save (second): %v", err)
	}

	got, err := store.Load(d.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Status != StatusPaused {
		t.Fatalf("status = %q, want %q", got.Status, StatusPaused)
	}
	if got.Completed() != 1024 {
		t.Fatalf("completed = %d, want 1024", got.Completed())
	}
	if got.UpdatedAt.Before(got.CreatedAt) {
		t.Fatal("UpdatedAt moved backwards")
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ID != d.ID {
		t.Fatalf("List returned %d records, expected the one just saved", len(items))
	}

	if err := store.Delete(d.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := store.Delete(d.ID); err != nil {
		t.Fatalf("Delete must be idempotent, got %v", err)
	}
	items, err = store.List()
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("List returned %d records after the delete", len(items))
	}
}

func TestStoreListSurvivesCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	store := Store{Dir: dir}

	good := &Download{ID: "good", URL: "https://example.test/a", Status: StatusQueued}
	if err := store.Save(good); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "broken.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write corrupt record: %v", err)
	}

	items, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].ID != "good" {
		t.Fatalf("a corrupt record hid the healthy ones: %+v", items)
	}
}
