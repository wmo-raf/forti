package dataset

import (
	"time"

	"github.com/metno/forti/rawdataforecaster/internal/server/forecast/dataset/values"
)

// LocationData is a struct for holding timeseries data and metadata for a single location from a specific dataset.
type LocationData struct {
	Meta *Meta
	Data []values.LocationDataCollection

	// Area is the name the answering dataset is configured and requested
	// under, which is not necessarily Meta.Area: that one comes from the
	// dataset's own metadata and nothing checks the two agree.
	Area string

	GridLocation Location
}

type Location struct {
	Lat  float32
	Long float32
}

// GetData extracts all values for the given parameter
func (p *LocationData) GetData(parameter string) map[time.Time]float32 {
	for _, data := range p.Data {
		pm, ok := data.ParameterMeta[parameter]
		if !ok {
			return nil
		}
		ret := make(map[time.Time]float32)
		for i, t := range pm.Times {
			ret[t] = data.Data[pm.SliceFrom+i]
		}
		return ret
	}
	return nil
}
