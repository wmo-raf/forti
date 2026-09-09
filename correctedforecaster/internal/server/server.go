package server

import (
	"context"
	"fmt"
	"log"
	"math"
	"net"
	"time"

	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/metno/forti/correctedforecaster/internal/correction"
	"github.com/metno/forti/correctedforecaster/internal/download"
	"github.com/metno/forti/correctedforecaster/internal/health"
	"github.com/metno/forti/correctedforecaster/internal/lookup"
	"github.com/metno/forti/internal/internalprotocol"
	"github.com/metno/forti/internal/radar"
)

func Run(upstream string, port int, topography download.Grouped) error {
	server, err := New(upstream, topography)
	if err != nil {
		return err
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return err
	}

	s := grpc.NewServer(
		grpc.UnaryInterceptor(grpc_prometheus.UnaryServerInterceptor),
	)
	internalprotocol.RegisterForecasterServer(s, server)
	grpc_health_v1.RegisterHealthServer(s, health.NewServer(server.client))

	grpc_prometheus.EnableHandlingTimeHistogram()
	grpc_prometheus.Register(s)

	return s.Serve(lis)
}

type Server struct {
	internalprotocol.UnimplementedForecasterServer

	// topo is the default topography: the files at the root of the work
	// directory. It answers a request that names no set, and lies underneath
	// every named set as a fallback.
	topo *lookup.Collection

	// topoSets are the named sets, one per subdirectory of the work
	// directory. Kept apart from topo rather than in the same map under an
	// empty key, so that a request naming "" cannot reach the default.
	topoSets map[string]*lookup.Collection

	conn   *grpc.ClientConn
	client internalprotocol.ForecasterClient
}

func New(upstream string, topography download.Grouped) (*Server, error) {
	conn, err := grpc.Dial(upstream,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithUnaryInterceptor(grpc_prometheus.UnaryClientInterceptor),
	)
	if err != nil {
		return nil, fmt.Errorf("could not to upstream: %w", err)
	}

	client := internalprotocol.NewForecasterClient(conn)

	topo, topoSets, err := openTopography(topography)
	if err != nil {
		return nil, err
	}

	return &Server{
		topo:     topo,
		topoSets: topoSets,
		conn:     conn,
		client:   client,
	}, nil
}

// openTopography opens one Collection per group: the default set from the
// files at the root, and one named set per subdirectory.
func openTopography(topography download.Grouped) (*lookup.Collection, map[string]*lookup.Collection, error) {
	topo := lookup.NewCollection()
	if err := topo.Add(topography[download.DefaultSet]...); err != nil {
		return nil, nil, fmt.Errorf("unable to add topography: %w", err)
	}
	log.Printf("default topography: %d files", len(topography[download.DefaultSet]))

	topoSets := make(map[string]*lookup.Collection)
	for _, name := range topography.Names() {
		files := topography[name]
		c := lookup.NewCollection()
		if err := c.Add(files...); err != nil {
			return nil, nil, fmt.Errorf("unable to add topography set %s: %w", name, err)
		}
		log.Printf("topography set %s: %d files", name, len(files))
		topoSets[name] = c
	}

	return topo, topoSets, nil
}

func waitForUpstream(client internalprotocol.ForecasterClient) {
	var ctx context.Context
	request := internalprotocol.GetForecastRequest{
		Latitude:  60,
		Longitude: 10,
	}
	for {
		_, err := client.GetForecast(ctx, &request)
		if err == nil {
			break
		}
		time.Sleep(5 * time.Second)
	}
}

func (s *Server) Close() error {
	return s.conn.Close()
}

func (s *Server) GetForecast(ctx context.Context, in *internalprotocol.GetForecastRequest) (*internalprotocol.Forecast, error) {
	forecast, err := s.client.GetForecast(ctx, in)
	if err != nil {
		log.Printf("unable to get forecast from rawdataforecaster: %s", err)
		return nil, fmt.Errorf("unable to get forecast from upstream: %w", err)
	}

	if forecast.ForecastStatus != internalprotocol.ForecastStatus_OK {
		return forecast, nil
	}

	if err := s.correct(in, forecast); err != nil {
		return nil, err
	}

	return forecast, nil
}

func (s *Server) correct(request *internalprotocol.GetForecastRequest, forecast *internalprotocol.Forecast) error {
	interpreted := internalprotocol.InterpretValues(forecast)
	if err := s.correctWithBetterTopography(request, interpreted); err != nil {
		return err
	}
	if err := s.correctWithRadarStatus(request, interpreted, forecast); err != nil {
		return err
	}
	return nil
}

func (s *Server) correctWithBetterTopography(request *internalprotocol.GetForecastRequest, interpreted map[string]internalprotocol.InterpretedData) error {
	modelAltitude, ok := getAltitude(interpreted)
	if !ok {
		return nil
	}

	var realAltitude float32
	if request.Altitude != nil && request.Altitude.Override {
		realAltitude = request.Altitude.Value
	} else {
		found, err := s.lookupTopography(request.Topography, float64(request.Latitude), float64(request.Longitude))
		if err != nil {
			log.Printf("unable to lookup topography for location: %s", err)
			return fmt.Errorf("unable to lookup topography for location: %w", err)
		}
		if found == nil {
			// No topography covers this point, so there is nothing to correct
			// towards. Taking the model's own altitude leaves the difference
			// at zero and the forecast uncorrected, which is what has always
			// been served where a DEM ends.
			realAltitude = *modelAltitude
		} else {
			realAltitude = *found
		}
		realAltitude = float32(math.Round(float64(realAltitude)))
	}

	altitudeDiff := *modelAltitude - realAltitude
	// Only correct when the altitude difference exceeds ±100 m. Smaller gaps are
	// within the noise of the model grid and not worth adjusting.
	if altitudeDiff < -100 || altitudeDiff > 100 {
		correction.UpdateTemperature(interpreted, altitudeDiff)
		correction.UpdateDewpointTemperature(interpreted)
		correction.UpdateSymbols(interpreted)
	}
	*modelAltitude = realAltitude

	return nil
}

// lookupTopography returns the real ground altitude at a point from the first
// collection that has data there, or nil if none does.
//
// The named sets come first, in the order the request gave them, and the
// default set last: a consumer supplies its own fine DEM and gets the global
// one underneath it for free. Priority within a set is still filename order,
// so the existing naming convention keeps working inside each group.
func (s *Server) lookupTopography(sets []string, latitude, longitude float64) (*float32, error) {
	for _, c := range s.topographyFor(sets) {
		val, err := c.Lookup(latitude, longitude)
		if err != nil {
			if lookup.IsOutOfBounds(err) {
				continue
			}
			return nil, err
		}
		return &val, nil
	}
	return nil, nil
}

// topographyFor returns the collections to consult for a request naming the
// given sets, in the order to consult them. No names means the default set
// alone, which is what a request that does not ask for one carries.
//
// An unrecognised name yields nothing at all -- not even the default. Falling
// back to the default would hand the caller terrain data it did not ask for
// and has no way to detect, and returning an error would turn a typo into an
// outage. With nothing to consult the lookup misses and the forecast degrades
// to uncorrected, which is what is already served wherever no DEM reaches.
//
// One unrecognised name discards the recognised ones with it, rather than
// being skipped over. Skipping it would leave a request for ["a", "typo"]
// answered exactly like a request for ["a"] -- which is the "ignore unknown
// names" option, the one rejected above, arriving by the back door. Whether
// the caller then notices depends on whether the sets it did name happened to
// cover the point. Refusing the whole request instead means a typo shows up
// the same way wherever it is in the list.
//
// Note that this is the opposite of what an unknown *area* does, which fails
// closed to OutsideAllGrids. The asymmetry is deliberate: a missing grid
// means there is no forecast to give, while a missing DEM means there is one,
// just uncorrected.
func (s *Server) topographyFor(sets []string) []*lookup.Collection {
	// Guarded rather than assumed: openTopography always builds a default
	// collection, but a Server assembled field by field -- as the tests do --
	// need not have one, and a nil Collection would panic rather than miss.
	var byDefault []*lookup.Collection
	if s.topo != nil {
		byDefault = []*lookup.Collection{s.topo}
	}
	if len(sets) == 0 {
		return byDefault
	}

	collections := make([]*lookup.Collection, 0, len(sets)+1)
	for _, name := range sets {
		c, ok := s.topoSets[name]
		if !ok {
			log.Printf("no topography set named %q: serving an uncorrected forecast", name)
			return nil
		}
		collections = append(collections, c)
	}
	return append(collections, byDefault...)
}

func getAltitude(interpreted map[string]internalprotocol.InterpretedData) (*float32, bool) {
	altitude, ok := interpreted["altitude"]
	if !ok {
		return nil, false
	}
	return &altitude.Values[0], true
}

// correctWithRadarStatus removes all values for lwe_precipitation_rate if radar coverage is not ok.
func (s *Server) correctWithRadarStatus(request *internalprotocol.GetForecastRequest, interpreted map[string]internalprotocol.InterpretedData, forecast *internalprotocol.Forecast) error {
	if status, ok := interpreted["precipitation_status"]; ok {
		if radar.Coverage(status.Values[0]) != radar.OK {
			for _, m := range forecast.ParameterMeta {
				if m.Parameter == "lwe_precipitation_rate" {
					// This should cause all lookups to be of size 0
					m.Times = nil
					return nil
				}
			}
		}
	}
	return nil
}
