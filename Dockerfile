FROM golang:1.27-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/aggregator ./cmd/aggregator

FROM alpine:3.20

RUN apk add --no-cache ca-certificates && adduser -D -u 10001 aggregator
WORKDIR /app
# configs/ ships alongside the binary because "discover"'s default seed
# file path (configs/seed_companies.json) is relative — it's resolved
# against the process's working directory, not the binary's location.
COPY --from=build /out/aggregator /usr/local/bin/aggregator
COPY --from=build /src/configs ./configs
RUN chown -R aggregator:aggregator /app
USER aggregator

ENTRYPOINT ["/usr/local/bin/aggregator"]

CMD ["run"]
