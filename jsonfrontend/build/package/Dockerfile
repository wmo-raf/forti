FROM golang:1.26.6

COPY go.mod /src/go.mod
COPY go.sum /src/go.sum
WORKDIR /src
RUN go mod download

COPY internal/ /src/internal/
COPY jsonfrontend/internal /src/jsonfrontend/internal
COPY jsonfrontend/pkg /src/jsonfrontend/pkg
COPY jsonfrontend/cmd /src/jsonfrontend/cmd

# The tests read the shipped jsonformat.json out of cmd/, so cmd/ has to be
# here before they run.
RUN go test ./...

WORKDIR /src/jsonfrontend/cmd/jsonfrontend
RUN go build


FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

EXPOSE 8080

# Configuration is mounted at /config and managed outside the image, so that
# changing it does not require a rebuild. This copy is a starting point for
# populating a fresh config volume, not something the service reads.
COPY jsonfrontend/cmd/jsonfrontend/jsonformat.json /usr/share/forti/jsonformat.json.example
COPY --from=0 /src/jsonfrontend/cmd/jsonfrontend/jsonfrontend /usr/local/bin

# The service runs as nobody, reads /config, and reports what it made of the
# configuration by writing /status/<module>.json. Docker gives a fresh named
# volume the ownership of the image directory it is mounted over, so both
# directories have to exist here, owned by nobody, before that happens.
#
# Without it the service still runs and every report is silently lost — a
# config manager reading the directory can only conclude the service is not
# there. /config is owned by nobody for the same reason: the config manager
# writes it as the same unprivileged user, and whichever image first mounts a
# fresh volume decides its ownership for both.
RUN mkdir -p /status /config && chown nobody:nogroup /status /config

USER nobody

ENTRYPOINT [ "/usr/local/bin/jsonfrontend" ]
CMD [ "-config", "/config/jsonformat.json", "-status-dir", "/status" ]
