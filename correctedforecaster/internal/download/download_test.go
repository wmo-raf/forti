package download

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"gocloud.dev/blob"
	_ "gocloud.dev/blob/memblob"
)

func TestDownloadValidKeyReturnsFilename(t *testing.T) {
	ctx := context.Background()
	bkt, err := blob.OpenBucket(ctx, "mem://")
	if err != nil {
		t.Fatalf("opening mem bucket: %v", err)
	}
	defer bkt.Close()

	const key = "forecast"
	const content = "abc"
	if err := bkt.WriteAll(ctx, key, []byte(content), nil); err != nil {
		t.Fatalf("writing object: %v", err)
	}

	baseDir := t.TempDir()
	toDir := filepath.Join(baseDir, "data")
	if err := os.Mkdir(toDir, 0o755); err != nil {
		t.Fatalf("creating target dir: %v", err)
	}

	obj := &blob.ListObject{
		Key:  key,
		Size: int64(len(content)),
	}

	filename, err := download(ctx, bkt, obj, toDir)
	if err != nil {
		t.Fatalf("download returned error: %v", err)
	}

	expected := filepath.Join(toDir, key)
	if filename != expected {
		t.Fatalf("unexpected filename: got %q, want %q", filename, expected)
	}

	f, err := os.Open(filename)
	if err != nil {
		t.Fatalf("opening downloaded file: %v", err)
	}
	defer f.Close()

	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(b) != content {
		t.Fatalf("unexpected file content: got %q, want %q", string(b), content)
	}
}

func TestDownloadRejectsPathTraversalKey(t *testing.T) {
	baseDir := t.TempDir()
	toDir := filepath.Join(baseDir, "data")
	if err := os.Mkdir(toDir, 0o755); err != nil {
		t.Fatalf("creating target dir: %v", err)
	}

	obj := &blob.ListObject{
		Key:  "../escape.txt",
		Size: 3,
	}

	_, err := download(context.Background(), nil, obj, toDir)
	if err == nil {
		t.Fatal("expected error for path traversal key, got nil")
	}

	escapedPath := filepath.Join(baseDir, "escape.txt")
	if _, statErr := os.Stat(escapedPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no file to be created outside target dir, stat err: %v", statErr)
	}
}

// writeFiles creates each named file, relative to dir, creating parent
// directories as needed.
func writeFiles(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating parent of %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
}

// The regression that matters most: a flat directory has to keep listing
// exactly what it listed before, in the same order.
func TestListDirFlatDirectoryIsUnchanged(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "20-global.tif", "10-region.tif")

	files, err := ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}

	want := []string{
		filepath.Join(dir, "10-region.tif"),
		filepath.Join(dir, "20-global.tif"),
	}
	if !slices.Equal(files, want) {
		t.Errorf("got %v, want %v", files, want)
	}
}

// ListDir used ReadDir and skipped directories, so a grouped layout was
// invisible to a restart without -download-from: the first boot downloaded and
// served the nested files, and the next one silently served none of them.
func TestListDirRecursesIntoSubdirectories(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "20-global.tif", "consumer-a/10-region.tif", "consumer-b/nested/deep.tif")

	files, err := ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}

	want := []string{
		filepath.Join(dir, "20-global.tif"),
		filepath.Join(dir, "consumer-a", "10-region.tif"),
		filepath.Join(dir, "consumer-b", "nested", "deep.tif"),
	}
	slices.Sort(want)
	if !slices.Equal(files, want) {
		t.Errorf("got %v, want %v", files, want)
	}
}

// ListDir handed GDAL every non-directory file it found. A .aux.xml sidecar,
// a README or a stray .DS_Store fails lookup.Open, which fails Collection.Add,
// which fails server.New, which main.go turns into a log.Fatalf. One stray
// file and there is no service at all.
func TestListDirSkipsFilesGDALCannotRead(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir,
		"10-region.tif",
		"10-region.tif.aux.xml",
		"20-global.nc",
		"README",
		"notes.md",
		".DS_Store",
	)

	files, err := ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}

	want := []string{
		filepath.Join(dir, "10-region.tif"),
		filepath.Join(dir, "20-global.nc"),
	}
	if !slices.Equal(files, want) {
		t.Errorf("got %v, want %v", files, want)
	}
}

// The filter is on the extension, not on the name, so an unusual but valid
// raster is still served. Narrowing this list silently drops working data,
// which is why every skip is logged.
func TestListDirKeepsLessCommonRasterFormats(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, "a.vrt", "b.img", "c.hgt", "d.TIFF", "e.bil", "f.asc")

	files, err := ListDir(dir)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}

	if len(files) != 6 {
		t.Errorf("expected all six rasters, got %v", files)
	}
}

// A blob key may contain slashes. os.Create does not create the directories
// leading to a file, so a nested key failed the download outright -- which
// main.go turns into a log.Fatalf. Grouping topography by subdirectory makes
// nested keys the normal case rather than an oddity.
func TestDownloadCreatesParentDirectoriesForNestedKey(t *testing.T) {
	ctx := context.Background()
	bkt, err := blob.OpenBucket(ctx, "mem://")
	if err != nil {
		t.Fatalf("opening mem bucket: %v", err)
	}
	defer bkt.Close()

	const key = "consumer-a/10-region.tif"
	const content = "abc"
	if err := bkt.WriteAll(ctx, key, []byte(content), nil); err != nil {
		t.Fatalf("writing object: %v", err)
	}

	toDir := t.TempDir()
	filename, err := download(ctx, bkt, &blob.ListObject{Key: key, Size: int64(len(content))}, toDir)
	if err != nil {
		t.Fatalf("download returned error: %v", err)
	}

	expected := filepath.Join(toDir, filepath.FromSlash(key))
	if filename != expected {
		t.Fatalf("unexpected filename: got %q, want %q", filename, expected)
	}
	b, err := os.ReadFile(filename)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(b) != content {
		t.Fatalf("unexpected file content: got %q, want %q", string(b), content)
	}
}
