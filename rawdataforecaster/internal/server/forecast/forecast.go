// Package forecast provides the latest forecast for a given location.
package forecast

import (
	"context"
	"errors"
	"log"
	"slices"
	"sync"
	"time"

	internalformat "github.com/metno/forti-internalformat"

	"github.com/metno/forti/rawdataforecaster/internal/server/config"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset/index/lookup"
	"github.com/prometheus/client_golang/prometheus"
)

// Forecast gives the latest weather forecast for a location.
type Forecast struct {
	store internalformat.Client

	cfg *config.Configuration

	datasets map[string]*dataset.Dataset
	m        sync.RWMutex
}

// New initializes an object that can be queries for forecasts. It is self-updating.
func New(cfg *config.Configuration) (*Forecast, error) {
	store, err := internalformat.NewClient(cfg.Source.Bucket)
	if err != nil {
		return nil, err
	}

	f, err := newFromClient(store, cfg)
	if err != nil {
		return nil, err
	}

	go f.run()

	return f, nil
}

func newFromClient(store internalformat.Client, cfg *config.Configuration) (*Forecast, error) {
	f := &Forecast{
		store:    store,
		cfg:      cfg,
		datasets: make(map[string]*dataset.Dataset),
	}

	if err := f.update(); err != nil {
		return nil, err
	}

	return f, nil
}

// ErrOutsideAllGrids is returned by Forecast.Get if no grids can be found for the given latitude and longitude.
var ErrOutsideAllGrids = errors.New("outside all grids")

// ErrPointTooFarAway is returned by Forecast.Get if a maximum distance to
// returned grid point is defined, and the point has a greater distance
// than that.
var ErrPointTooFarAway = errors.New("returned point is too far away")

// Get returns a forecast for the given latitude and longitude. Returns
// ErrOutsideAllGrids if outside all grids, or ErrPointTooFarAway if grid
// point is too far away.
//
// areas narrows the search to the named areas. An empty areas is the whole
// loaded set, which is what a request that does not ask for one carries, so
// the filter can only ever narrow an answer and never widen one.
//
// It comes from the request rather than from the configuration on purpose:
// the configuration says which areas the process holds, and can be replaced
// under the lock at any moment, while this says which of them one caller will
// accept. Reading it here keeps the filter out of the reload path entirely.
func (f *Forecast) Get(latitude, longitude float32, areas []string) (*dataset.LocationData, error) {
	f.m.RLock()
	defer f.m.RUnlock()

	best, err := f.bestArea(latitude, longitude, areas)
	if err != nil {
		return nil, err
	}

	areaCounter.With(prometheus.Labels{"area": best.Area}).Inc()

	locationData, err := best.D.Read(latitude, longitude)
	if err != nil {
		return nil, err
	}
	locationData.GridLocation = best.GridLocation
	locationData.Area = best.Area

	return locationData, nil
}

type bestDataset struct {
	D *dataset.Dataset

	// Area is the key the dataset is held under, which is the name the
	// configuration listed and so the name a request filters on. Meta.Area is
	// a different string: it is read from the dataset's own complete.json and
	// nothing checks the two agree. Reporting the key back means the answer a
	// caller audits is the same name it asked for, even if the stored
	// metadata is mislabelled.
	Area string

	GridLocation dataset.Location
}

func (f *Forecast) bestArea(latitude, longitude float32, areas []string) (*bestDataset, error) {
	var selected *dataset.Dataset
	var selectedName string
	var selectedLocation *lookup.GeoResponse
	for name, area := range f.datasets {
		if !selects(areas, name) {
			continue
		}
		closestLocation, err := area.ClosestGridLocation(latitude, longitude)
		if err != nil {
			return nil, err
		}
		if selectedLocation == nil || closestLocation.Distance < selectedLocation.Distance {
			selected = area
			selectedName = name
			selectedLocation = closestLocation
		}
	}

	if selected == nil {
		if len(areas) > 0 {
			// Every name the caller gave is unloaded, or was dropped from the
			// configuration since. Answering from the areas it excluded would
			// make a typo silently widen the query, which is the whole failure
			// this filter exists to prevent, so this fails closed instead: a
			// request that matches nothing is outside all the grids it asked
			// for. That is a path callers already handle, and it needs no new
			// status value on the wire.
			return nil, ErrOutsideAllGrids
		}
		return nil, errors.New("no datasets available")
	}
	if !selected.WithinGeographicArea(latitude, longitude) {
		return nil, ErrOutsideAllGrids
	}
	if !selected.ResponseHasAcceptableDistance(selectedLocation) {
		return nil, ErrPointTooFarAway
	}

	distanceHistogram.With(prometheus.Labels{"area": selectedName}).Observe(float64(selectedLocation.Distance))

	return &bestDataset{
		D:            selected,
		Area:         selectedName,
		GridLocation: dataset.Location{Lat: selectedLocation.Lat, Long: selectedLocation.Long},
	}, nil
}

// selects reports whether an area may answer a request narrowed to filter.
//
// An empty filter selects everything. proto3 decodes an absent repeated field
// to a nil slice, so that case is exactly a request that did not ask to be
// narrowed, and needs no sentinel to tell the two apart.
func selects(filter []string, area string) bool {
	return len(filter) == 0 || slices.Contains(filter, area)
}

// SetConfig puts a new configuration into use.
//
// Areas that are no longer listed are dropped and their readers closed. update
// only ever adds, so without this a removed area would go on being served from
// memory until the process restarted. Newly listed areas need nothing here:
// the next poll finds them, three seconds away.
//
// Everything else in the configuration is read when an area is downloaded, or
// not again at all — see config.ColdChanges, which is what tells the operator
// so rather than leaving them to wonder why nothing happened.
func (f *Forecast) SetConfig(cfg *config.Configuration) {
	wanted := make(map[string]bool, len(cfg.Areas))
	for _, area := range cfg.Areas {
		wanted[area] = true
	}

	f.m.Lock()
	f.cfg = cfg

	var dropped []*dataset.Dataset
	for area, ds := range f.datasets {
		if wanted[area] {
			continue
		}
		delete(f.datasets, area)
		dropped = append(dropped, ds)
		log.Printf("dropped: %s", area)
		forgetArea(area)
	}
	f.m.Unlock()

	// Closed after the lock is released. A dataset is only reachable through
	// the map, and taking the write lock waited out every request already
	// reading one, so nothing can be using these — and freeing their memory
	// should not hold up the requests waiting to start.
	for _, ds := range dropped {
		if err := ds.Close(); err != nil {
			log.Printf("closing dropped dataset: %s", err)
		}
	}
}

func (f *Forecast) run() {
	for {
		time.Sleep(3 * time.Second)
		if err := f.update(); err != nil {
			// log errors, and retry on next round
			log.Printf("%s - will retry later", err)
		}
	}
}

// update looks up new data from blob store, and loads it.
func (f *Forecast) update() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	latest, err := f.store.Latest(ctx)
	if err != nil {
		return err
	}

	// One snapshot for the whole round, threaded down rather than read again
	// inside load: the configuration can be replaced at any moment, and a
	// download that read it twice could use two different loaders.
	f.m.RLock()
	cfg := f.cfg
	toAdd := make(map[string]int)
	for _, area := range cfg.Areas {
		// A configured area with no marker has never published, which is an
		// ordinary state: the config manager can name an area before the
		// producer's first upload finishes. A map miss reads as version 0, so
		// without this it was queued for load, GetMeta(area, 0) failed, and
		// update reported "unable to load some areas" — which at startup is
		// log.Fatalf, taking every other area in the process down with it.
		//
		// Only a missing marker is skipped. A marker naming a version whose
		// data is not in the bucket is still a load failure and still errors.
		latestVersion, published := latest[area]
		if !published {
			continue
		}
		current, ok := f.datasets[area]
		if !ok || current.Meta.Version < latestVersion {
			toAdd[area] = latestVersion
		}
	}
	f.m.RUnlock()

	errs := make(chan error)
	for area, version := range toAdd {
		go func(area string, version int) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			errs <- f.load(ctx, cfg, area, version)
		}(area, version)
	}
	hasLoadErrors := false
	for area, version := range toAdd {
		if err := <-errs; err != nil {
			log.Printf("unable to load %s/%d: %s", area, version, err)
			hasLoadErrors = true
		}
	}

	if hasLoadErrors {
		return errors.New("unable to load some areas")
	}
	return nil
}

func (f *Forecast) load(ctx context.Context, cfg *config.Configuration, area string, version int) error {
	log.Printf("available: %s/%d", area, version)

	prometheusLabel := prometheus.Labels{"area": area}

	fortiAvailableLatest.With(prometheusLabel).Set(float64(version))
	fortiAvailableUpdated.With(prometheusLabel).Set(float64(time.Now().Unix()))

	meta, err := f.store.GetMeta(ctx, area, version)
	if err != nil {
		return err
	}

	ds, err := dataset.Download(ctx, f.store, meta, cfg)
	if err != nil {
		return err
	}

	f.m.Lock()
	// The configuration may have been replaced while this was downloading. An
	// area dropped in the meantime must not come back, or removing one from
	// the config manager would look like it had worked and then undo itself.
	if !slices.Contains(f.cfg.Areas, area) {
		f.m.Unlock()
		ds.Close()
		// The available gauges were set above, before the download. Nothing
		// should still be reporting a version for an area that was dropped.
		forgetArea(area)
		log.Printf("discarding %s/%d: no longer configured", area, version)
		return nil
	}
	replaced := f.datasets[area]
	f.datasets[area] = ds

	// Set under the same lock that SetConfig drops these gauges under.
	// Outside it, a SetConfig removing this area in the window between the
	// unlock and here would run forgetArea first, and this would then put back
	// a gauge saying an area is active that is no longer served.
	fortiActiveLatest.With(prometheusLabel).Set(float64(version))
	fortiActiveUpdated.With(prometheusLabel).Set(float64(time.Now().Unix()))
	f.m.Unlock()

	// Closed after the lock is released, as in SetConfig: it is out of the map
	// and every reader of it has finished.
	if replaced != nil {
		if err := replaced.Close(); err != nil {
			log.Printf("closing replaced dataset %s: %s", area, err)
		}
	}

	log.Printf("active: %s/%d", area, version)

	return nil
}
