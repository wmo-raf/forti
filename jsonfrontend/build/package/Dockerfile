FROM golang:1.26.6

COPY go.mod /src/go.mod
COPY go.sum /src/go.sum
WORKDIR /src
RUN go mod download

COPY internal/ /src/internal/
COPY jsonfrontend/internal /src/jsonfrontend/internal
COPY jsonfrontend/pkg /src/jsonfrontend/pkg

RUN go test ./...

COPY jsonfrontend/cmd /src/jsonfrontend/cmd

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

USER nobody

ENTRYPOINT [ "/usr/local/bin/jsonfrontend" ]
CMD [ "-config", "/config/jsonformat.json", "-status-dir", "/status" ]
