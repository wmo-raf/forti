# rawdataforecaster

Serves raw (uncorrected) forecast data over gRPC from a blob store (Azure, S3, or local `file://`) in the [forti-internalformat](https://github.com/metno/forti-internalformat) format.

## Usage

See `rawdataforecaster --help` for command-line options.

## Configuration

The configuration file is JSON. Example:

```json
{
  "source": {
    "bucket": "file:///data/forecast"
  },
  "areas": ["meps"],
  "loader": {
    "type": "blob"
  },
  "maximum_gridpoint_distance": 50000
}
```

### Configuration Options

#### `source`

Defines where forecast data is stored.

- **`source.bucket`** (string, required): Bucket URL where forecast datasets are stored. Supported schemes:
  - `file:///absolute/path` — Local filesystem
  - `azblob://container-name` — Azure Blob Storage (requires Azure credentials)
  - `s3://bucket-name` — Amazon S3 (requires AWS credentials)

#### `areas`

- **`areas`** (array of strings, required): List of forecast area identifiers to load. Each area corresponds to a subdirectory in the bucket containing forecast datasets in forti-internalformat. Example values:
  - `"meps"` — MetCoOp Ensemble Prediction System (Nordic region)
  - `"tanzania"` — Some forecast for Tanzania

#### `loader`

Defines how forecast data is loaded and cached.

- **`loader.type`** (string, required): Loading strategy. See [Loader strategies](#loader-strategies) below.
- **`loader.configuration`** (object, optional): Strategy-specific options (see below).

#### `maximum_gridpoint_distance`

- **`maximum_gridpoint_distance`** (integer, optional): Maximum distance in meters from a requested point to the nearest grid cell. Points farther away return a "point too far away" error. If omitted, will return the nearest point, regardless of distance.

## Loader strategies

The `loader.type` config key selects how forecast data is held in memory:

| Type | Behaviour | Use When |
|---|---|---|
| `memory` | Downloads the full grid blob at startup into CGo-allocated memory (bypassing GC pressure). Zero I/O at query time. | Latency matters and the dataset fits in RAM |
| `blob` | Downloads nothing upfront; issues a byte-range read per query (2 s timeout). | Dataset is too large for RAM, or startup time matters |

Use `memory` when latency matters and the dataset fits in RAM; use `blob` otherwise.

### `loader.configuration`

#### `max_size_gib`

Caps how much a single area is allowed to download. The check runs before any
data is fetched, so an area over its limit is skipped and the previously loaded
version of it stays in use.

The limit applies to **both loader types**, not just `memory`, and it is
compared against the **total of all grids in the area**, not each grid
separately.

```json
{
  "loader": {
    "type": "blob",
    "configuration": {
      "max_size_gib": {
        "nowcast": 0.2,
        "world": 12
      }
    }
  }
}
```

- **`max_size_gib`** (object, optional): Maximum download size in GiB, keyed by
  area name. An area with no key here is not capped. The value must be a
  positive number.

It is an object rather than a single number because one rawdataforecaster
instance commonly serves several areas of very different sizes — `arctic`,
`nordic` and `world` in the same file — which a single limit could not cap
separately.

> Earlier revisions of this document showed `"max_size_gib": 4`, a bare number.
> That shape has never been accepted by the code, and until recently it crashed
> the process on startup. It is now rejected with an error naming the shape
> above.

## Status file

`--status-dir` makes the process write `rawdataforecaster.json` there after
every configuration attempt, and every 15 seconds while its state changes. The
file is written by rename, so a reader never sees half of one.

```json
{
  "module": "rawdataforecaster",
  "loaded_sha": "9f86d0…",
  "loaded_at": "2026-09-09T17:00:00Z",
  "ok": true,
  "applied": ["areas"],
  "state": {
    "areas": [
      {"area": "nordic", "available": 178891200015, "loaded": 178891200015},
      {"area": "arctic", "available": 178891200015, "loaded": null},
      {"area": "world",  "available": null,         "loaded": null}
    ]
  }
}
```

Everything above `state` describes the configuration file: which one was read
and whether it was accepted. `state` describes the process, and answers the
question the rest cannot — a configuration applies just as cleanly when the
areas it names are not being served.

Each area reports `available`, the version the store's `latest/` marker named
at the last successful listing, and `loaded`, the version being served. Both are `null` when there is none, so
the three cases above read directly: `nordic` is serving what the store
offers, `arctic` has data waiting that it has not managed to download, and
`world` has never been published at all. A `loaded` behind an `available` for
more than a few minutes is the same problem as `arctic`'s, caught earlier.

`available` does not decay, so a `store_error` appears alongside `areas`
whenever the last attempt to list the store failed. Without it a bucket that
has become unreachable would read as every area serving exactly what is on
offer — the healthiest possible report of the one failure that stops any area
ever being updated again.

Only configured areas appear. An area in the store that this process was not
asked to serve is not its business.

## Native dependencies

The geographic index uses [s2geometry](https://github.com/google/s2geometry) and [PROJ](https://proj.org/) via CGo. These are pre-installed in the devcontainer; building outside it requires both libraries.
