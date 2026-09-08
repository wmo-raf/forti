package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"sync"

	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/fileblob"
	_ "gocloud.dev/blob/s3blob"

	"github.com/metno/forti/internal/configwatch"
	"github.com/metno/forti/rawdataforecaster/internal/server"
	"github.com/metno/forti/rawdataforecaster/internal/server/config"
	"github.com/metno/forti/rawdataforecaster/internal/server/forecast"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func main() {
	confFile := flag.String("config", "config.json", "Path to JSON configuration file (defines data source, areas, loader strategy)")
	stats := flag.Bool("serve-stats", false, "Enable Prometheus metrics endpoint on :8080/metrics")
	port := flag.Int("port", 5052, "gRPC server listen port")
	statusDir := flag.String("status-dir", "", "Directory to write config reload status to, for a config manager to read (empty to disable)")

	flag.Parse()

	reloader := &reloader{}
	watcher := &configwatch.Watcher{
		Module:    "rawdataforecaster",
		Path:      *confFile,
		StatusDir: *statusDir,
		Apply:     reloader.apply,
	}

	// Startup and reload validate the same way, so a configuration that would
	// be rejected on reload never starts the service either.
	if err := watcher.Load(); err != nil {
		log.Fatalf("unable to read configuration: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	if *stats {
		go serveStats()
	}

	log.Fatalln(server.Run(reloader.running(), *port))
}

// reloader puts configuration files into effect.
//
// The first file has to build the forecaster and later ones have to change it,
// but both go through apply, so they are validated and reported the same way.
// Building here rather than in main is what lets a bucket that cannot be
// opened reach the status file: reporting startup as successful and then
// exiting would leave a config manager looking at a green status file behind a
// dead process, which is this codebase's own bug pattern.
type reloader struct {
	mu sync.Mutex

	// booted is the configuration the forecaster was constructed from. Cold
	// settings are compared against this rather than against the previous
	// file, because "needs a restart" describes the running process, not the
	// last edit. Diffing edit against edit would drop the warning on the next
	// unrelated save while the process was still using the old value.
	booted     *config.Configuration
	forecaster *forecast.Forecast
}

// running returns the forecaster, which exists once apply has succeeded once.
func (r *reloader) running() *forecast.Forecast {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.forecaster
}

// apply validates a configuration file and puts it into use. It never touches
// the running configuration when it finds problems: the forecaster carries on
// with what it has, and the status file says why.
func (r *reloader) apply(data []byte) configwatch.Result {
	conf, problems := config.Parse(data)
	if len(problems) != 0 {
		return configwatch.Result{Errors: problems}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.forecaster == nil {
		log.Printf("use %s storage strategy", conf.Loader.Type)

		forecaster, err := forecast.New(conf)
		if err != nil {
			return configwatch.Result{Errors: []error{
				fmt.Errorf("source.bucket: %w", err)}}
		}
		r.forecaster = forecaster
		r.booted = conf

		return configwatch.Result{
			Applied: []string{"source", "areas", "loader", "maximum_gridpoint_distance"},
		}
	}

	r.forecaster.SetConfig(conf)

	return configwatch.Result{
		Applied:        []string{"areas"},
		PendingRestart: config.ColdChanges(r.booted, conf),
	}
}

func serveStats() {
	log.Println("serving prometheus stats on http://localhost:8080/metrics")
	http.Handle("/metrics", promhttp.Handler())
	log.Println(http.ListenAndServe(":8080", nil))
}
