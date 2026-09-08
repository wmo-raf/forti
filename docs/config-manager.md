# forti-control: Config Manager Design

Status: **agreed**. Slice 1 (jsonfrontend worker side), slice 2a
(`forti-control` MVP) and slice 3 (rawdataforecaster) **implemented**.
Date: 2026-09-08

A web UI for managing forti's config files, with in-place reload — modelled on
nginx-proxy-manager, but with the filesystem rather than a database as the
source of truth.

---

## 1. Goal

Today, changing forti config means editing a file and redeploying. For
`jsonfrontend` it is worse: the config is baked into the image
(`jsonfrontend/build/package/Dockerfile:24` does `COPY jsonformat.json /`), so a
parameter-mapping change requires a rebuild.

We want a UI that edits config and has the running services pick it up, without
a redeploy and without a restart.

## 2. Architecture

```
                 ┌───────────────┐
  browser ──────▶│ forti-control │  Go + htmx, local users,
                 └───────┬───────┘  snapshots + audit log
             writes │        │ reads
                    ▼        ▲
        /config/*.json   /status/*.json          (shared volume)
                    │        ▲
            watches │        │ writes
        ┌───────────┴────────┴────────────┐
        │ jsonfrontend   rawdataforecaster │  healthz
        └──────────────────────────────────┘
              correctedforecaster — not managed
```

**The filesystem is the entire interface.** The controller has no network
connection to any worker, no RPC, no service discovery, no credentials, and no
Docker socket. It writes files and reads files.

Consequences:

- `forti-control` has **no Go dependency on forti**. No shared proto, no version
  skew, no two-repo release dance.
- Identical behaviour on Docker Compose, plain VMs, and Kubernetes.
- Workers need no inbound surface, so nothing new is exposed.

### Why not SIGHUP

The original plan was nginx-style: write the file, send `SIGHUP`. That works for
nginx-proxy-manager because **NPM ships the manager and nginx in one container**,
so `nginx -s reload` is a local signal.

Forti's workers are separate containers with separate PID namespaces. A process
in the controller container has no PID to signal. Delivering a signal would
require mounting the Docker socket into the controller (root-equivalent on the
host) or sharing PID namespaces (Compose-only, fragile).

File-watching costs the same few lines in `main.go` as a signal handler, and
needs none of that.

### Why not a database, like NPM

In NPM the database is authoritative and the nginx `.conf` files are generated,
disposable artifacts regenerated from templates on every change.

That suits NPM because its config is flat rows (`forward_ip`, `forward_port`).
Forti's configs are nested hand-shaped documents — `jsonformat.json` is a
two-level parameter map. Generating them from DB columns would put forti's
semantics inside the controller. Files stay authoritative instead.

---

## 3. Decisions

| Area | Decision |
|---|---|
| Managed modules | `jsonfrontend`, `rawdataforecaster`, `healthz` |
| Not managed | `correctedforecaster` (flags-only), `xmlfrontend`, `moxfrontend` (out of scope) |
| Reload trigger | Worker watches its own config file |
| Watch mechanism | `stat` mtime+size on a ~1s timer (see note below) |
| Write protocol | Controller writes temp file then `rename()` — atomic |
| Validation | Worker validates before applying; never trusts the file |
| Bad config | Keep serving the old config; report errors in the status file |
| Ack channel | Worker writes `/status/<module>.json` |
| Cold fields | Applied what it can; rest reported as restart-required |
| Read safety | One config snapshot per request, threaded explicitly |
| History | Timestamped snapshots + audit log, linked by snapshot hash |
| Schema | Hand-written in the controller |
| Unknown fields | Merged and preserved; shown in an advanced raw-JSON box |
| Auth | Local users, hashed passwords, seeded admin |
| Controller stack | Go, server-rendered, htmx, `go:embed`, separate repo |

### Note on fsnotify vs polling

Use polling. With atomic writes (`temp` + `rename`), **fsnotify must watch the
config file's directory, not the file** — `rename` replaces the inode, so a
watch on the file itself silently stops firing after the first change. This is
the standard bug in this pattern.

A 1-second `stat` on mtime+size has none of that subtlety, is a fraction of the
code, and is entirely adequate for config that changes a few times a week.

### Status file contract

```json
{
  "loaded_sha":      "abc123...",
  "loaded_at":       "2026-09-08T14:02:11Z",
  "ok":              false,
  "errors":          ["loader.type: unknown value \"blobb\""],
  "applied":         ["areas"],
  "pending_restart": ["source.bucket"]
}
```

Written after **every** load attempt, successful or not. The UI polls it via
htmx and shows the verdict about a second after Save.

### History contract

- Snapshot the **new** content after each successful write, and snapshot
  whatever already exists at first controller startup — so history contains
  every version including the current one, and the pre-controller baseline is
  not lost.
- The audit entry **references the snapshot** by filename or hash.

That linkage is the thing NPM lacks. Its `audit_log.meta` holds only the
submitted patch (`_.assign({}, {domain_names: row.domain_names}, data)` in
`internal/proxy-host.js`), with no snapshot, so its log cannot answer "what did
it say before?" — and NPM has no rollback at all. Verified against its schema:
across migrations from 2018 to 2026 there is no versioning table.

### Unknown fields — why merge matters

The controller's schemas are hand-written, so they will lag forti. If the UI
built the file from form values, any field the schema does not model would be
**silently deleted** on the next save — someone edits `offer_gzip` and quietly
strips `location_from_grid` from production.

So: parse the existing file into a generic map, apply only what the form
changed, serialize the map back. Never regenerate from the form. Surface
unmodelled keys in an advanced raw-JSON panel — NPM's `advanced_config` answer —
so new forti fields are usable immediately without a controller release.

---

## 4. Per-module reload analysis

### jsonfrontend — trivial

`config.Initialize()` already re-assigns the package global
(`internal/server/config/config.go:37`), and `server.New()` caches nothing from
config. **Every field is read fresh per request.** So every field is hot.

One real problem: the global is read at **11 sites** across `server.go` and
`encode.go` with no synchronisation. A single request reads it ~6 times, so a
naked pointer swap lets one response straddle two config versions — and
`SkipAltitude` is read at both `server.go:162` and `encode.go:23`, which would
produce a response whose geometry contradicts itself.

Fix: snapshot once at the top of `ServeHTTP` and thread it down, following the
existing `expires(current, maxOffset)` precedent at `server.go:202`.

### healthz — small

`Health.conf` is a pointer field and the struct already carries a
`sync.RWMutex` (`internal/health/health.go:15`). The probe loop calls
`runProbe(h.conf)` fresh each minute (`:39`).

Add a setter guarded by the existing mutex; the next probe picks it up within
60s. `probeHistory` may need trimming if `probe_history.size` shrinks.

### rawdataforecaster — moderate, and partly cold — DONE

Config is read in `main.go`, passed to `server.Run` → `forecast.New` → captured
as `Forecast.cfg`. Reloading means swapping that field under the existing
`RWMutex`.

| field | tier | why |
|---|---|---|
| `areas` (add) | **hot, free** | the existing 3s poll loop picks up new areas once `cfg` is swapped — zero new code |
| `areas` (remove) | ~10 lines | `update()` never deletes dropped areas; needs a delete + `Close()` |
| `maximum_gridpoint_distance` | cold | baked into every `Dataset.GridLimit` at download |
| `loader.type` / `.configuration` | cold | consumed per-download |
| `source.bucket` | coldest | consumed by `internalformat.NewClient` in `New()` |

**Prerequisite — remove the panics.** An unknown `loader.type` panicked, and
`max_size_gib` was read through two unchecked type assertions. Those were
startup crashes with an operator watching. Under reload, a panic in the watch
path **kills a healthy serving process because someone typo'd a web form.**
Checked assertions and a total validator were non-negotiable here.

(Related: the README documented `max_size_gib` as a number, but the code
requires an object keyed by area — which is what `nowcast.json` actually ships.
Following the README panicked the process.)

Done in slice 3; see below for what landed.

### correctedforecaster — not managed

It has no config file because **it has no policy to configure**. Its flags are
all wiring (`-upstream`, `-workdir`, `-port`); its behaviour is hardcoded — the
lapse rate is a literal `0.006` at `internal/correction/temperature_correct.go:16`,
the parameter names are `const`s in `parameters.go:5-14`, the Magnus
coefficients are literals at `:150`.

That reasoning holds for one deployment. For a second region it breaks: `0.006`
is tuned for Norway, and `parameters.go` hardcodes MET Norway's internal
parameter names. Worth revisiting if forti is deployed outside Norway — see bug
2 below.

---

## 5. First increment — jsonfrontend end-to-end

Exercises every part of the design (write → watch → validate → swap → report →
history) on the cheapest module, concentrating the effort in the controller
where the real unknowns are.

### forti — DONE

| file | change |
|---|---|
| `internal/configwatch/` | **new.** Shared `Watcher` (stat mtime+size, then compare digest), `Applier`, `Result`, `Status`, atomic status writes. Reused by rawdataforecaster and healthz in later slices |
| `jsonfrontend/internal/server/config/config.go` | `atomic.Pointer` + `Current()`/`Set()`; `Parse()` returning `[]error`; `Problems()` validator |
| `jsonfrontend/internal/server/server.go` | one snapshot in `ServeHTTP`, threaded down; 5 global reads removed |
| `jsonfrontend/internal/server/encode/encode.go` | config passed as a parameter; 6 global reads removed |
| `jsonfrontend/cmd/jsonfrontend/main.go` | `-status-dir` flag; startup and reload share one validation path |
| `jsonfrontend/build/package/Dockerfile` | config no longer baked in; shipped as `/usr/share/forti/jsonformat.json.example`; `CMD` names `/config` and `/status` |

Verified end-to-end against the real binary: startup load, live reload, a bad
config rejected while the process keeps serving the previous one, then
recovery on the next good write — no restart at any point. All tests pass
under `-race`.

**What `Problems()` catches.** Every check is for a mistake that would
otherwise pass silently, because jsonfrontend drops what it cannot match
rather than complaining:

- two periods sharing an `offset` — `getTimeStep` keys periods by offset, so
  one is dropped, and which one varies between requests
- two internal parameters mapping to the same external name — `Details` is
  keyed by external name, so one is lost
- a `summary` with no `symbol_code`; a period with neither parameters nor a
  summary; an empty external name; a negative `data_expiry_offset`

A regression test asserts the **shipped `jsonformat.json` still validates**, so
the validator cannot quietly become stricter than production.

### forti-control — slice 2a DONE

Lives in `../forti-control`, module `github.com/metno/forti-control`, with no
Go dependency on forti. Login and seeded admin, module list, raw JSON editor,
atomic save, content-addressed snapshots, audit log in SQLite linked to them by
digest, one-click restore, and a status panel polling `/status/*.json`.

Verified end-to-end twice: against local processes, and against both services
in containers on real Docker volumes with the asymmetric mounts. Editing
`jsonformat.json` in a browser and saving reaches a running `jsonfrontend`
within a second; saving two periods sharing an offset shows the service's own
rejection message while it keeps serving the previous version.

The generated forms and merge-don't-regenerate semantics are 2b, still to do.

---

## 6. Remaining slices

Suggested order: **2a -> 3 -> 2b -> 4.** 2a and 3 are done, so `pending_restart`
is now real before there is a UI for it. **2b is next.**

### Slice 2 — forti-control MVP

The slice that makes slice 1 worth anything. Until it exists the reload path is
only drivable by hand-editing files, which was already possible.

**2a — the loop.** Login and seeded admin; list the managed modules; raw JSON
editor; save via temp + `rename()`; snapshot plus audit entry linked by hash;
status panel polling `/status/*.json`. The whole design proven with a text box.

**2b — the forms.** Hand-written schemas for the three modules; generated
forms; merge-don't-regenerate on save; advanced raw-JSON box for unmodelled
keys.

Forces three of the four open questions: audit log storage, whether
restore-from-snapshot is one click, and whether `USER nobody` can write
`/status` on the real mounts.

**Sequencing wrinkle — resolved.** The restart-required badge could not be
tested until slice 3, because `jsonfrontend` has no cold fields and nothing
populated `pending_restart`. It was built in 2a and first exercised in slice 3,
by changing `source.bucket`.

### Slice 3 — rawdataforecaster — DONE

**Started with the bug fixes, not the watcher.** Three panics were reachable
from config data: under reload, a panic in the watch path kills a healthy
serving process because somebody typo'd a web form.

| location | problem | now |
|---|---|---|
| `configuration.go:28` | `panic("no such downloader: " + type)` | `DownloadFunction` returns an error naming the accepted values |
| `dataset.go:50` | unchecked `maxSizes.(map[string]interface{})` | `Loader.MaxSizesGiB` checks it |
| `dataset.go:109` | unchecked `maxGiB.(float64)` | checked per area, reporting every bad value |

`max_size_gib` settled on **the code's shape, an object keyed by area**, and the
README was rewritten. One instance serves areas of very different sizes —
`forecast.json` covers arctic, nordic and world — so a single number could not
cap them separately, and the object is what `nowcast.json` ships. The README was
wrong in two further ways nobody had noticed: the limit applies to every loader,
not just `memory`, and it is compared against the area's total rather than each
grid.

| file | change |
|---|---|
| `internal/server/config/configuration.go` | `DownloadFunction` returns an error; `Loader.MaxSizesGiB` replaces both type assertions |
| `internal/server/config/reload.go` | **new.** `Parse` returning `[]error`, `Problems()`, `ColdChanges()` |
| `internal/server/forecast/forecast.go` | `SetConfig`; one config snapshot per poll round, threaded into `load`; `load` refuses to resurrect an area removed while it was downloading |
| `internal/server/forecast/prometheus.go` | `forgetArea` drops the gauges of a removed area |
| `internal/server/server.go` | `Run` takes the forecaster rather than building it, so `main` keeps the handle a reload needs |
| `cmd/rawdataforecaster/main.go` | `-status-dir`; `configwatch.Watcher`; startup and reload share one validation path |
| `build/package/Dockerfile` | config no longer implied; `/config` and `/status` created for `nobody`; `cmd/` copied before `go test` |

**`maximum_gridpoint_distance` was left cold**, and is reported in
`pending_restart` alongside `source.bucket` and `loader.*`. Lifting it to the
live check was considered and declined: the badge tells the operator the truth
either way, and it keeps `Dataset.GridLimit` and its test untouched.

**What the reload actually covers.** Only `areas` is live. `source.bucket` is
never consulted again — the store is opened once in `New` — and the loader
settings are read when an area is downloaded, so a change reaches each area
only when its next version lands, hours away and one area at a time. Half
applied is not applied, so `ColdChanges` reports all of them.

**What `Problems()` catches**, following the same rule as jsonfrontend's — every
check is for a mistake that would otherwise pass silently:

- a `max_size_gib` key naming an area not in `areas`, so the limit caps nothing
- `areas` empty, which answers every request with `no datasets available`
- an area listed twice, or named `""`
- an unknown or missing `loader.type`; `max_size_gib` of the wrong shape
- a negative `maximum_gridpoint_distance`

A regression test asserts the three shipped configs still validate, so the
validator cannot quietly become stricter than production. That test reads
`cmd/`, which is why the Dockerfile now copies `cmd/` before running the tests
— the same trap jsonfrontend hit in slice 1.

**Verified end-to-end** against a running binary and forti-control on a shared
directory: adding an area starts it being served within one poll, removing one
stops it and frees its readers, changing `source.bucket` shows the
restart-required badge — its first real exercise, since jsonfrontend has no cold
fields — and a config that would previously have panicked leaves the service on
its old configuration with the reason in the status file.

**Caveat:** this module cannot be built or tested outside the devcontainer — cgo
against s2geometry and PROJ.

**Left alone, found while working here.** `dataset.Download` leaks the readers
it has already opened if `index.Add` fails (`dataset.go:78`); the loop closes
them on a download error but not on an index error. Pre-existing, and reload
makes it more reachable, but it is not this slice.

**One deployment limit.** The status file is named after the module, so the
three rawdataforecaster instances a full deployment runs — nowcast, forecast,
oceanforecast — would all write `rawdataforecaster.json`. The controller's
registry has one entry for the module, so this matches what it can manage
today. Managing more than one instance needs a name per instance on both sides.

### Slice 4 — healthz

Smallest of the three. `h.conf` is read at exactly four sites — `health.go:40`,
`:52`, `:125`, `:136` — and three are already under the existing mutex. Add a
`SetConfig` taking the write lock, and snapshot `conf` under `RLock` at the top
of `Probe()`; line 40's `runProbe(h.conf)` is the one read currently outside it.

`config.Problems()` already exists at `config.go:79`, so the validator is free.

Two things to watch: `probeHistory` needs trimming if `probe_history.size`
shrinks, and reload only takes effect on the next probe, up to 60s later — the
status file should say so rather than implying it was instant.

### Slice 5 — correctedforecaster, only if forti leaves Norway

Lift the hardcoded policy — lapse rate `0.006`, the parameter-name `const`s, the
Magnus coefficients — into a config file. Corrections run per request, so it
would be entirely hot.

The forcing function is not config management, it is bug 2 below: against a
pipeline with different parameter names, `UpdateSymbols` silently applies
nothing and returns raw data labelled as corrected.

---

## 7. Open questions

All settled. The file-watch interval is 1s (`configwatch.DefaultInterval`).
The four that blocked slice 2a were decided as part of it:

- **Audit log storage — SQLite**, through `modernc.org/sqlite`, which is
  cgo-free, so the controller still builds anywhere. It holds the users, the
  sessions and the audit log. The configuration itself stays in files: the
  database holds only the controller's own bookkeeping.
- **Restore — one click**, with an inline confirmation. A restore is an
  ordinary save of old bytes: same atomic write, same audit entry with a
  different action, and it reuses the snapshot it came from rather than
  copying it. So a restore is itself undoable and history stays append-only.
- **`/status` writability — confirmed, and it needed a fix.** Tested against a
  real Docker volume: `USER nobody` **could not** write `/status`, because
  Docker gives a fresh named volume the ownership of the image directory it is
  mounted over, and that directory did not exist. The service ran fine and
  every report was silently lost — this codebase's own bug pattern. The
  Dockerfile now creates `/status` and `/config` owned by `nobody`. `/config`
  is mounted read-only into services and read-write into the controller;
  `/status` the other way round.
- **Where `forti-control` lives — `../forti-control`**, its own git repo,
  module `github.com/metno/forti-control`, no remote yet. No Go dependency on
  forti: the contract is the two JSON documents on the volume.

### Found while driving slice 1 from the UI

`jsonfrontend`'s Dockerfile ran `go test ./...` before copying
`jsonfrontend/cmd`, but slice 1's new regression test reads the shipped
`jsonformat.json` out of `cmd/`. The image could not build at all. The `COPY`
now comes first.

---

## 8. Pre-existing bugs found

Not caused by this work, but all three are the same shape — **wrong config
produces silence, not an error** — which is exactly the failure class a config
UI makes easier to trigger.

1. **`jsonfrontend/internal/server/encode/encode.go:171`** — `getTimeStep`
   iterates the parameters the *data* has and looks each up in the config map.
   A parameter named in config but absent from the data is silently dropped from
   the API response. No error, no log. `getSummary` at `:191` behaves the same
   way with `symbol_code`.

2. **`correctedforecaster/internal/correction/temperature_correct.go:34`** —
   `UpdateSymbols` does `if symbols, ok := interpreted[weatherSymbol1h]; ok`.
   Against a pipeline using different parameter names it applies **no
   corrections at all** and returns raw data labelled as corrected.

3. **`healthz/internal/health/config/` test fixture and live config** assert on
   `air_temerature` (sic). A typo in a probe assertion silently passes.

4. **`docs/getting-started.md:56`** documents
   `go run ./jsonfrontend/cmd/jsonfrontend -upstream localhost:5052` with no
   `-config`. That looks for `jsonformat.json` in the repo root, where none
   exists, so the documented command has always failed. Confirmed by running
   it. (Now moot in containers, which name the path explicitly, but the doc
   still needs a `-config` flag.)

Fixing (1) and (2) — or at least logging them — would make the UI meaningfully
safer, since these are the changes most likely to be made from it.
