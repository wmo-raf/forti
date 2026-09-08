package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/pprof"

	"github.com/metno/forti/internal/configwatch"
	"github.com/metno/forti/jsonfrontend/internal/server"
	"github.com/metno/forti/jsonfrontend/internal/server/config"
	"github.com/metno/forti/jsonfrontend/internal/server/metrics"
)

func main() {
	upstream := flag.String("upstream", "localhost:5051", "gRPC upstream server address (rawdataforecaster or correctedforecaster)")
	metricsPort := flag.Int("metricsPort", 9090, "Prometheus metrics HTTP port (0 to disable)")
	profilePort := flag.Int("profilePort", 0, "pprof CPU profiling HTTP port (0 to disable)")
	configFile := flag.String("config", "jsonformat.json", "Path to JSON configuration file (defines parameter mapping, time periods, HTTP headers)")
	statusDir := flag.String("status-dir", "", "Directory to write config reload status to, for a config manager to read (empty to disable)")
	flag.Parse()

	watcher := &configwatch.Watcher{
		Module:    "jsonfrontend",
		Path:      *configFile,
		StatusDir: *statusDir,
		Apply:     apply,
	}

	// Startup and reload validate the same way, so a configuration that would
	// be rejected on reload never starts the service either.
	if err := watcher.Load(); err != nil {
		log.Fatalf("unable to read configuration: %s", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watcher.Run(ctx)

	server, err := server.New(*upstream)
	if err != nil {
		log.Fatalln(err)
	}

	if *metricsPort != 0 {
		log.Printf("serving stats at port %d", *metricsPort)
		addr := fmt.Sprintf(":%d", *metricsPort)
		go func() {
			log.Fatalln(metrics.Serve(addr))
		}()
	}

	if *profilePort != 0 {
		log.Printf("serving cpu profiles at port %d", *profilePort)
		addr := fmt.Sprintf(":%d", *profilePort)
		go func() {
			r := http.NewServeMux()
			r.HandleFunc("/debug/pprof/profile", pprof.Profile)
			log.Fatalln(http.ListenAndServe(addr, r))
		}()
	}

	http.Handle("/", server)
	log.Println("ready")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// apply validates a configuration file and puts it into use. Every setting
// jsonfrontend has is read while a request is being served, so there is
// nothing here that needs a restart.
func apply(data []byte) configwatch.Result {
	conf, problems := config.Parse(data)
	if len(problems) != 0 {
		return configwatch.Result{Errors: problems}
	}

	config.Set(conf)

	return configwatch.Result{Applied: []string{"parameters", "http_headers", "response options"}}
}
