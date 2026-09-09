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

// Get downloads all files from the given blob storage into the given folder.
func Get(ctx context.Context, fromBlob, toDir string) ([]string, error) {
	if fromBlob == "" {
		log.Printf("no blob address given - just using contents of folder %s as data source", toDir)
		return ListDir(toDir)
	}

	bkt, err := blob.OpenBucket(ctx, fromBlob)
	if err != nil {
		return nil, err
	}
	defer bkt.Close()

	var files []string
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
		files = append(files, f)
	}

	sort.Strings(files)
	return files, nil
}

// ListDir returns every file under dir, at any depth.
//
// It recurses because the bucket path does. A bucket key may contain slashes,
// and Get returns the nested path it downloaded to; a later restart without
// -download-from re-scans the same directory and has to find the same files.
// Listing only the top level meant the first boot served nested data and the
// next one silently served none of it.
func ListDir(dir string) ([]string, error) {
	var files []string
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
		files = append(files, path)
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}

func download(ctx context.Context, bkt *blob.Bucket, obj *blob.ListObject, toDir string) (string, error) {
	filename := filepath.Join(toDir, obj.Key)

	if !fileWithinDir(filename, toDir) {
		return "", fmt.Errorf("could not verify that file %s is within target directory %s", filename, toDir)
	}

	stat, err := os.Stat(filename)
	if os.IsNotExist(err) || stat.Size() != obj.Size {
		log.Println("download " + filename)
		f, err := os.Create(filename)
		if err != nil {
			return "", err
		}
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
	return filename, err
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
