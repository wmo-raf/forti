package main

import (
	"context"
	"flag"
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
	conf := reloader.config()

	log.Printf("use %s storage strategy", conf.Loader.Type)

	forecaster, err := forecast.New(conf)
	if err != nil {
		log.Fatalln(err)
	}
	reloader.attach(forecaster)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	if *stats {
		go serveStats()
	}

	log.Fatalln(server.Run(forecaster, *port))
}

// reloader applies configuration files to the running forecaster.
//
// It exists because the first configuration has to be validated before there
// is anything to apply it to. Startup and reload therefore take the same path
// through apply, which is the point: a file that reload would refuse must not
// be one that startup quietly accepts.
type reloader struct {
	mu sync.Mutex

	current    *config.Configuration
	forecaster *forecast.Forecast
}

// config returns the configuration last applied.
func (r *reloader) config() *config.Configuration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// attach hands over the running forecaster, after which reloads reach it.
func (r *reloader) attach(f *forecast.Forecast) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forecaster = f
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

	previous := r.current
	r.current = conf

	if r.forecaster == nil {
		// Startup. There is nothing to apply this to yet, but everything in it
		// is about to be used to build the forecaster.
		return configwatch.Result{
			Applied: []string{"source", "areas", "loader", "maximum_gridpoint_distance"},
		}
	}

	r.forecaster.SetConfig(conf)

	return configwatch.Result{
		Applied:        []string{"areas"},
		PendingRestart: config.ColdChanges(previous, conf),
	}
}

func serveStats() {
	log.Println("serving prometheus stats on http://localhost:8080/metrics")
	http.Handle("/metrics", promhttp.Handler())
	log.Println(http.ListenAndServe(":8080", nil))
}
