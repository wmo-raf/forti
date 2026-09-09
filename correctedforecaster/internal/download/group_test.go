package download

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	_ "gocloud.dev/blob/fileblob"
)

// The regression that matters most: with no subdirectories present, grouping
// has to be byte-for-byte what a flat directory did before it existed. The
// compatibility guarantee needs no flag; it falls out of the layout.
func TestGroupFlatDirectoryIsAllDefault(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "20-global.tif", "10-region.tif")

	sets, err := Get(context.Background(), "", dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if len(sets) != 1 {
		t.Fatalf("expected only the default set, got %v", sets)
	}
	want := []string{
		filepath.Join(dir, "10-region.tif"),
		filepath.Join(dir, "20-global.tif"),
	}
	if !slices.Equal(sets[DefaultSet], want) {
		t.Errorf("default set is %v, want %v", sets[DefaultSet], want)
	}
}

// Files at the root are the default set; each subdirectory is a named set.
// Priority within a set is still alphabetical, so the existing naming
// convention keeps working unchanged inside every group.
func TestGroupGroupsBySubdirectory(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"20-global.tif",
		"consumer-a/10-region.tif",
		"consumer-a/05-city.tif",
		"consumer-b/nested/deep.tif",
	)

	sets, err := Get(context.Background(), "", dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	assertSet(t, sets, DefaultSet, filepath.Join(dir, "20-global.tif"))
	assertSet(t, sets, "consumer-a",
		filepath.Join(dir, "consumer-a", "05-city.tif"),
		filepath.Join(dir, "consumer-a", "10-region.tif"))
	// Grouping is on the first path segment only, however deep the file sits.
	assertSet(t, sets, "consumer-b", filepath.Join(dir, "consumer-b", "nested", "deep.tif"))
}

// A bucket prefix has to group the same way a directory does, or a deployment
// would behave differently on the boot that downloads and the boot that does
// not.
func TestGroupFromBucketMatchesDirectory(t *testing.T) {
	ctx := context.Background()

	src := t.TempDir()
	writeFiles(t, src, "20-global.tif", "consumer-a/10-region.tif", "consumer-a/README")

	dir := t.TempDir()
	downloaded, err := Get(ctx, "file://"+src, dir)
	if err != nil {
		t.Fatalf("Get from bucket: %v", err)
	}

	// The same directory re-scanned without a bucket has to yield the same
	// grouping. This is the restart that used to lose every nested file.
	rescanned, err := Get(ctx, "", dir)
	if err != nil {
		t.Fatalf("Get from directory: %v", err)
	}

	if len(downloaded) != len(rescanned) {
		t.Fatalf("bucket gave %v, rescan gave %v", downloaded, rescanned)
	}
	for name, files := range downloaded {
		if !slices.Equal(files, rescanned[name]) {
			t.Errorf("set %q: bucket gave %v, rescan gave %v", name, files, rescanned[name])
		}
	}
	assertSet(t, downloaded, DefaultSet, filepath.Join(dir, "20-global.tif"))
	// The README came down with the rest -- GDAL reads sidecars off the disk
	// -- but is not offered to it as a dataset.
	assertSet(t, downloaded, "consumer-a", filepath.Join(dir, "consumer-a", "10-region.tif"))
	if _, err := os.Stat(filepath.Join(dir, "consumer-a", "README")); err != nil {
		t.Errorf("README was not downloaded: %v", err)
	}
}

// All flattens for the tools that want every file in one collection. Root
// files stay first so a flat directory is unchanged there too.
func TestGroupedAllPutsTheDefaultSetFirst(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "20-global.tif", "consumer-b/b.tif", "consumer-a/a.tif")

	sets, err := Get(context.Background(), "", dir)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	want := []string{
		filepath.Join(dir, "20-global.tif"),
		filepath.Join(dir, "consumer-a", "a.tif"),
		filepath.Join(dir, "consumer-b", "b.tif"),
	}
	if !slices.Equal(sets.All(), want) {
		t.Errorf("got %v, want %v", sets.All(), want)
	}
}

func assertSet(t *testing.T, sets Grouped, name string, want ...string) {
	t.Helper()
	if !slices.Equal(sets[name], want) {
		t.Errorf("set %q is %v, want %v", name, sets[name], want)
	}
}
