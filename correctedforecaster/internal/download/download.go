package download

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gocloud.dev/blob"
)

// DefaultSet is the name of the group holding the files at the root of the
// work directory. It is the set consulted by a request that names none, and
// it lies underneath every named set as a fallback.
//
// A subdirectory cannot be called "", so no named set can collide with it.
const DefaultSet = ""

// Grouped is topography files grouped by the first path segment below the
// work directory. Files at the root are under DefaultSet; a file in
// consumer-a/, at any depth, is under "consumer-a".
//
// Within each group the files stay in sorted order, because Collection.Lookup
// takes the first file with data at a point and so priority is filename
// order. That convention is unchanged; it now applies within a group rather
// than across the whole directory.
type Grouped map[string][]string

// All returns every file in every group, the default set first and the named
// sets after it in name order. For a directory with no subdirectories this is
// exactly the flat list that Get used to return.
func (g Grouped) All() []string {
	names := make([]string, 0, len(g))
	for name := range g {
		if name != DefaultSet {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	files := append([]string(nil), g[DefaultSet]...)
	for _, name := range names {
		files = append(files, g[name]...)
	}
	return files
}

// Get downloads all files from the given blob storage into the given folder,
// grouped by the subdirectory they were found in.
func Get(ctx context.Context, fromBlob, toDir string) (Grouped, error) {
	if fromBlob == "" {
		log.Printf("no blob address given - just using contents of folder %s as data source", toDir)
		return ListDir(toDir)
	}

	bkt, err := blob.OpenBucket(ctx, fromBlob)
	if err != nil {
		return nil, err
	}
	defer bkt.Close()

	sets := make(Grouped)
	it := bkt.List(nil)
	for {
		obj, err := it.Next(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		f, err := download(ctx, bkt, obj, toDir)
		if err != nil {
			return nil, err
		}
		// Everything in the bucket is downloaded, but only rasters are
		// offered to GDAL as datasets. A .aux.xml sidecar has to be on disk
		// next to its raster for GDAL to pick it up, and must not be opened
		// as a dataset in its own right.
		if !isRaster(f) {
			log.Printf("skipping %s: not a recognised raster format", f)
			continue
		}
		sets.add(toDir, f)
	}

	sets.sort()
	return sets, nil
}

// add files f, which must be inside dir, to the group named by its first path
// segment below dir.
func (g Grouped) add(dir, f string) {
	rel, err := filepath.Rel(dir, f)
	if err != nil {
		// f came from filepath.Join(dir, ...) or from WalkDir under dir, so
		// this cannot happen. Grouping it as the default is the harmless
		// reading if it somehow does.
		log.Printf("cannot place %s relative to %s: %s", f, dir, err)
		g[DefaultSet] = append(g[DefaultSet], f)
		return
	}

	set := DefaultSet
	if i := strings.Index(rel, string(os.PathSeparator)); i >= 0 {
		set = rel[:i]
	}
	g[set] = append(g[set], f)
}

func (g Grouped) sort() {
	for _, files := range g {
		sort.Strings(files)
	}
}

// ListDir returns every file under dir, at any depth, grouped by the
// subdirectory it was found in.
//
// It recurses because the bucket path does. A bucket key may contain slashes,
// and Get returns the nested path it downloaded to; a later restart without
// -download-from re-scans the same directory and has to find the same files.
// Listing only the top level meant the first boot served nested data and the
// next one silently served none of it.
func ListDir(dir string) (Grouped, error) {
	sets := make(Grouped)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !isRaster(path) {
			log.Printf("skipping %s: not a recognised raster format", path)
			return nil
		}
		log.Println("serve " + path)
		sets.add(dir, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sets.sort()
	return sets, nil
}

func download(ctx context.Context, bkt *blob.Bucket, obj *blob.ListObject, toDir string) (string, error) {
	filename := filepath.Join(toDir, obj.Key)

	if !fileWithinDir(filename, toDir) {
		return "", fmt.Errorf("could not verify that file %s is within target directory %s", filename, toDir)
	}

	// A blob key may contain slashes, and os.Create will not create the
	// directories leading to the file. Done after the traversal check above,
	// so a key climbing out of toDir cannot create directories on its way.
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		return "", err
	}

	// Anything other than "it is not there" is a real error and has to be
	// reported: os.Stat leaves stat nil, so reading its size would panic.
	// Reachable with nested keys, where a file standing where a directory
	// belongs gives ENOTDIR rather than ENOENT.
	stat, err := os.Stat(filename)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if stat == nil || stat.Size() != obj.Size {
		log.Println("download " + filename)
		f, err := os.Create(filename)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r, err := bkt.NewReader(ctx, obj.Key, nil)
		if err != nil {
			return "", err
		}
		defer r.Close()
		if _, err := io.Copy(f, r); err != nil {
			return "", err
		}
		return filename, nil
	}

	log.Println(filename + " already exists")
	return filename, nil
}

// fileWithinDir checks that the given file is within the given directory, preventing path traversal attacks.
func fileWithinDir(file, dir string) bool {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return false
	}
	absFile, err := filepath.Abs(file)
	if err != nil {
		return false
	}

	return strings.HasPrefix(absFile, absDir+string(os.PathSeparator))
}

// rasterExtensions are the file extensions offered to GDAL as topography.
//
// An allowlist rather than a denylist, because the failure it prevents is
// fatal: anything GDAL cannot open fails lookup.Open, which fails
// Collection.Add, which fails server.New, which main.go turns into a
// log.Fatalf. One README next to the data and there is no service.
//
// The cost of the allowlist is the opposite failure -- a raster in a format
// not listed here is dropped, and a dropped raster is only visible as an
// uncorrected forecast. That is why the list is generous and why every skip
// is logged. Add to it rather than working around it.
var rasterExtensions = map[string]bool{
	".asc":  true, // ESRI ASCII grid
	".bil":  true, // band interleaved by line
	".bt":   true, // VTP binary terrain
	".dem":  true, // USGS DEM
	".dt0":  true, // DTED, three levels
	".dt1":  true,
	".dt2":  true,
	".grd":  true, // Surfer / GMT grid
	".gtx":  true, // NOAA vertical datum grid
	".h5":   true, // HDF5
	".hdf":  true,
	".hdf5": true,
	".hgt":  true, // SRTM
	".img":  true, // ERDAS Imagine
	".nc":   true, // NetCDF
	".nc4":  true,
	".tif":  true, // GeoTIFF
	".tiff": true,
	".vrt":  true, // GDAL virtual dataset
}

// isRaster reports whether the given file looks like something GDAL can open
// as topography. The check is on the extension alone: opening the file to find
// out needs GDAL, and the point is to keep unopenable files away from it.
func isRaster(filename string) bool {
	return rasterExtensions[strings.ToLower(filepath.Ext(filename))]
}
